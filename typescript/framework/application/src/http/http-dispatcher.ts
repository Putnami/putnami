import {
  HttpException,
  NotAcceptableException,
  runInContext,
  tryContext,
  useContext,
  useLogger,
} from '@putnami/runtime';
import { stableWireCode } from '../api/route/error-codes';
import { negotiateResponse } from './content-negotiation';
import { devErrorsEnabled, isDevHtmlRequest, renderDevErrorPage } from './dev-error-page';
import { buildHttpContext } from './http-context.builder';
import type { HttpRequestContext, HttpRequestContextInternal, Server } from './http-context.type';
import { HttpResponse } from './http-response';
import { getHttpStatusText } from './http-status.type';
import type { RouteController } from './route.controller';
import type { RouteHandler } from './route.type';

/**
 * A function that intercepts and optionally transforms errors.
 *
 * Return a value to use as the response body, or `undefined` to fall through
 * to the default error handling. Throwing from the handler replaces the
 * original error.
 */
export type ErrorHandler = (error: unknown, ctx: HttpRequestContext) => unknown;

/** Dispatches incoming HTTP requests to matched route handlers, with error handling and content negotiation. */
export class HttpDispatcher {
  errorHandler?: ErrorHandler;

  constructor(private router: RouteController<HttpRequestContext>) {}

  /** Builds an HTTP context from a raw request and dispatches it through the router. */
  async dispatch(req: Request, server: Server): Promise<undefined | HttpResponse> {
    const httpContext = buildHttpContext<HttpRequestContextInternal>({ req, server });
    return this.handle(httpContext);
  }

  /**
   * Match route and populate context with route/params.
   * Call this before running middlewares to give them access to routing info.
   * Caches the matched handlers on the context so handle() can reuse them.
   */
  matchRoute(httpContext: HttpRequestContextInternal): void {
    const handlers = this.router.find(httpContext);
    httpContext.__matchedHandlers = handlers;
    if (handlers.length > 0) {
      const first = handlers[0];
      httpContext.route = first.route;
      httpContext.params = first.params;
      httpContext.statusCode = first.statusCode;
      httpContext.__firstPartyErrors = first.firstPartyErrors;
    }
  }

  /** Iterates through matched handlers until one produces a response, populating route context on each attempt. */
  async handle(httpContext: HttpRequestContextInternal): Promise<undefined | HttpResponse> {
    // Reuse cached match from matchRoute() if available, otherwise find fresh
    const handlers = httpContext.__matchedHandlers ?? this.router.find(httpContext);

    // The HTTP plugin already wraps the whole request in runInContext(httpContext),
    // so re-entering AsyncLocalStorage with the same store per handler is redundant.
    // Only establish the context here when called outside one (e.g. the standalone
    // dispatch() entrypoint used by gRPC / testing).
    const alreadyInContext = tryContext() === httpContext;

    for (let i = 0; i < handlers.length; i++) {
      const handlerResult = handlers[i];
      httpContext.route = handlerResult.route;
      httpContext.params = handlerResult.params;
      httpContext.statusCode = handlerResult.statusCode;
      httpContext.__firstPartyErrors = handlerResult.firstPartyErrors;

      const handler = handlerResult.handler;
      try {
        const res = alreadyInContext
          ? await this.runHandler(handler, httpContext)
          : await runInContext(httpContext, () => this.runHandler(handler, httpContext));
        if (res) {
          return res;
        }
      } catch (e) {
        if (!(e instanceof NotAcceptableException)) {
          throw e;
        }
      }
    }

    return undefined;
  }

  private runHandler = async <CONTEXT extends HttpRequestContext = HttpRequestContext>(
    handler: RouteHandler<CONTEXT>,
    context: CONTEXT,
  ) => {
    let r: undefined | HttpResponse | Promise<HttpResponse>;
    try {
      const result = await handler(context);
      r = wrapResponse(result, context);
    } catch (e) {
      r = await this.handleError(e, context);
    }
    return flushContext(r);
  };

  private async handleError(e: unknown, context: HttpRequestContext): Promise<HttpResponse> {
    // Store on context so middleware (e.g. LoggerMiddleware) can log the error
    (context as HttpRequestContextInternal).__requestError = e;

    // Give the user's error handler first chance
    if (this.errorHandler) {
      try {
        const result = await this.errorHandler(e, context);
        if (result !== undefined) {
          // If the handler already returned an HttpResponse, honour its status.
          // Otherwise wrap as JSON 500 so error responses never accidentally 200.
          if (result instanceof HttpResponse) {
            return result;
          }
          return HttpResponse.json(result, { status: 500 });
        }
      } catch (handlerError) {
        // Error handler itself threw — log and fall through to defaults
        useLogger().error('Error handler threw:', handlerError);
      }
    }

    // Intentional HttpResponse (e.g. redirect()) — return as-is, not an error
    if (e instanceof HttpResponse) {
      return e;
    }

    // Dev error page — rich HTML overlay for browser requests in development
    if (isDevHtmlRequest(context)) {
      return renderDevErrorPage(e, context);
    }
    if (e instanceof HttpException) {
      const status = e.getStatus();
      if ((context as HttpRequestContextInternal).__firstPartyErrors) {
        return HttpResponse.json(firstPartyErrorBody(e, status), { status });
      }
      return HttpResponse.json(e.getResponse(), { status });
    }
    useLogger().error(e);
    const isDev = devErrorsEnabled();
    const body = isDev
      ? {
          error: e instanceof Error ? e.message : String(e),
          stack: e instanceof Error ? e.stack : undefined,
        }
      : { error: 'Internal Server Error' };
    return HttpResponse.json(body, { status: 500 });
  }
}

/** The first-party client-contract error envelope — identical in shape to `go/framework/errors/http.go`'s `HTTPErrorBody`. */
interface FirstPartyErrorBody {
  readonly code: string;
  readonly error: string;
  readonly message: string;
  readonly details?: unknown;
}

/**
 * Projects an `HttpException` into the first-party envelope: a stable wire `code`
 * (`stableWireCode`), the canonical HTTP status phrase as `error` (matching Go's
 * `WriteHTTPError`, which always uses `http.StatusText(status)` rather than a
 * caller-supplied description), the exception's own `message`, and — when the
 * exception was constructed with a plain details object rather than a string —
 * that object (minus the `statusCode`/`message`/`error` fields `createBody` adds)
 * as `details`.
 */
function firstPartyErrorBody(e: HttpException, status: number): FirstPartyErrorBody {
  const code = stableWireCode(e.code, status);
  const error = getHttpStatusText(status) ?? e.name;
  const response = e.getResponse();
  if (typeof response === 'object' && response !== null) {
    const {
      statusCode: _statusCode,
      message: _message,
      error: _error,
      ...details
    } = response as Record<string, unknown>;
    if (Object.keys(details).length > 0) {
      return { code, error, message: e.message, details };
    }
  }
  return { code, error, message: e.message };
}

/**
 * Wrap handler return value into HttpResponse.
 * - HttpResponse: return as-is
 * - string: wrap as text response
 * - object/array: negotiate content type based on Accept header
 * - undefined: return as-is (no response)
 */
const wrapResponse = (result: unknown, context: HttpRequestContext): HttpResponse | undefined => {
  if (result === undefined || result === null) {
    return undefined;
  }
  if (result instanceof HttpResponse) {
    return result;
  }
  if (typeof result === 'string') {
    return new HttpResponse(result);
  }
  // Object, array, or other — negotiate based on Accept header
  const accept = context.headers.get('Accept');
  return negotiateResponse(result, accept);
};

const flushContext = <R = undefined | HttpResponse | Promise<HttpResponse>>(r: R | undefined): R => {
  const cbs = useContext<ContextWithCallback>().__callbacks;
  if (cbs && r instanceof HttpResponse) {
    let rh = r as HttpResponse;
    for (const cb of cbs) {
      rh = cb(rh);
    }

    return rh as R;
  }

  return r as R;
};

type ContextWithCallback = {
  __callbacks?: ((r: HttpResponse) => HttpResponse)[];
};
