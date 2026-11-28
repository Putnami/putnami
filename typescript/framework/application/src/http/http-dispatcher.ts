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
import type { CompiledAnswer, RouteContinuation } from './compiled-route';
import { type EndpointAnswer, endpointAnswerOf } from './endpoint-answer';
import { buildHttpContext } from './http-context.builder';
import type { HttpRequestContext, HttpRequestContextInternal, Server } from './http-context.type';
import { HttpResponse } from './http-response';
import { getHttpStatusText } from './http-status.type';
import type { RouteController } from './route.controller';
import type { HandlerResult } from './router';
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
    this.useMatch(httpContext, this.router.find(httpContext));
  }

  /**
   * Records `handlers` as the request's match, as {@link matchRoute} does with
   * the router's: a route Bun's native router matched arrives with its match.
   */
  useMatch(httpContext: HttpRequestContextInternal, handlers: HandlerResult<RouteHandler<HttpRequestContext>>[]): void {
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
  handle(httpContext: HttpRequestContextInternal): Promise<undefined | HttpResponse> {
    let handlers: HandlerResult<RouteHandler<HttpRequestContext>>[];
    let alreadyInContext: boolean;
    try {
      // Reuse cached match from matchRoute() if available, otherwise find fresh
      handlers = httpContext.__matchedHandlers ?? this.router.find(httpContext);
      // The HTTP plugin already wraps the whole request in runInContext(httpContext),
      // so re-entering AsyncLocalStorage with the same store per handler is redundant.
      // Only establish the context here when called outside one (e.g. the standalone
      // dispatch() entrypoint used by gRPC / testing).
      alreadyInContext = tryContext() === httpContext;
    } catch (error) {
      return Promise.reject(error);
    }
    // One candidate, inside the request's context: no loop and no async frame of its own.
    if (handlers.length === 1 && alreadyInContext) {
      useCandidate(httpContext, handlers[0]);
      return this.runHandler(handlers[0].handler, httpContext).catch(skipNotAcceptable);
    }
    return this.handleEach(httpContext, handlers, alreadyInContext);
  }

  /**
   * Runs the one handler of a route compiled at start, inside the request's
   * context, as {@link handle} runs a single candidate, and hands its answer
   * to `continuation` in the same continuation: at once, with no promise,
   * when the handler answers at once. An error past the dispatcher's own
   * answer goes to `continuation.failed`. An endpoint is called through its
   * parts, so its empty answer adds no promise either.
   */
  runRoute(
    httpContext: HttpRequestContextInternal,
    handler: RouteHandler<HttpRequestContext>,
    continuation: RouteContinuation,
  ): CompiledAnswer {
    const parts = endpointAnswerOf(handler as RouteHandler);
    const call = parts === undefined ? handler : (parts.inner as RouteHandler<HttpRequestContext>);
    let result: ReturnType<RouteHandler<HttpRequestContext>>;
    try {
      result = call(httpContext);
    } catch (e) {
      return this.routeError(e, httpContext, continuation);
    }
    if (isThenable(result)) {
      return Promise.resolve(result).then(
        (value) => this.routeValue(parts, value, httpContext, continuation),
        (e: unknown) => this.routeError(e, httpContext, continuation),
      );
    }
    return this.routeValue(parts, result, httpContext, continuation);
  }

  /** The answer of a compiled route's handler that gave `value`, as `runHandler` wraps and flushes it. */
  private routeValue(
    parts: EndpointAnswer | undefined,
    value: unknown,
    httpContext: HttpRequestContextInternal,
    continuation: RouteContinuation,
  ): CompiledAnswer {
    let wrapped: HttpResponse | undefined;
    try {
      wrapped = wrapResponse(parts === undefined ? value : parts.answer(httpContext, value), httpContext);
    } catch (e) {
      return this.routeError(e, httpContext, continuation);
    }
    let flushed: HttpResponse | undefined;
    try {
      flushed = flushRequest(httpContext, wrapped);
    } catch (e) {
      return continuation.failed(e);
    }
    return continuation.answered(flushed);
  }

  /** The answer of a compiled route's handler that failed with `e`, as `runHandler` answers it. */
  private routeError(
    e: unknown,
    httpContext: HttpRequestContextInternal,
    continuation: RouteContinuation,
  ): Promise<HttpResponse | undefined> {
    return this.handleError(e, httpContext).then(
      (answer) => {
        let flushed: HttpResponse | undefined;
        try {
          flushed = flushRequest(httpContext, answer);
        } catch (x) {
          return continuation.failed(x);
        }
        return continuation.answered(flushed);
      },
      (x: unknown) => continuation.failed(x),
    );
  }

  private async handleEach(
    httpContext: HttpRequestContextInternal,
    handlers: HandlerResult<RouteHandler<HttpRequestContext>>[],
    alreadyInContext: boolean,
  ): Promise<undefined | HttpResponse> {
    for (let i = 0; i < handlers.length; i++) {
      const handlerResult = handlers[i];
      useCandidate(httpContext, handlerResult);

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

  /**
   * Runs one handler and wraps its result, or answers its error. A handler
   * that answers synchronously goes through no async frame.
   */
  private runHandler = <CONTEXT extends HttpRequestContext = HttpRequestContext>(
    handler: RouteHandler<CONTEXT>,
    context: CONTEXT,
  ): Promise<HttpResponse | undefined> => {
    const answerError = (e: unknown) => this.handleError(e, context).then(flushContext);
    let result: ReturnType<RouteHandler<CONTEXT>>;
    try {
      result = handler(context);
    } catch (e) {
      return answerError(e);
    }
    if (isThenable(result)) {
      return Promise.resolve(result).then((value) => {
        let wrapped: HttpResponse | undefined;
        try {
          wrapped = wrapResponse(value, context);
        } catch (e) {
          return answerError(e);
        }
        return flushContext(wrapped);
      }, answerError);
    }
    let wrapped: HttpResponse | undefined;
    try {
      wrapped = wrapResponse(result, context);
    } catch (e) {
      return answerError(e);
    }
    try {
      return Promise.resolve(flushContext(wrapped));
    } catch (e) {
      return Promise.reject(e);
    }
  };

  /**
   * Calls the user's error handler for an error whose request already ended:
   * the answer stays 408. Returns whether the handler answered the error.
   */
  private async notifyErrorHandler(e: unknown, context: HttpRequestContext): Promise<boolean> {
    if (!this.errorHandler) return false;
    try {
      return (await this.errorHandler(e, context)) !== undefined;
    } catch (handlerError) {
      useLogger().error('Error handler threw:', handlerError);
      return false;
    }
  }

  private async handleError(e: unknown, context: HttpRequestContext): Promise<HttpResponse> {
    // The request ended (it passed its timeout or its client left): the HTTP
    // plugin answers 408. Work that stopped with it did not fail; any other
    // error is still the handler's own failure: it is recorded, passed to the
    // error handler and logged as it would be during the request.
    if (requestEnded(context) && !(e instanceof HttpResponse)) {
      if (!isRequestStop(e, context)) {
        recordRequestError(context, e);
        const answered = await this.notifyErrorHandler(e, context);
        if (!answered && !(e instanceof HttpException)) useLogger().error(e);
      }
      return HttpResponse.json({ error: 'Request Timeout', statusCode: 408 }, { status: 408 });
    }
    recordRequestError(context, e);

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

const REQUEST_STOP_NAMES = new Set(['AbortError', 'TimeoutError', 'QueryAbortError', 'ClientCanceledError']);

/** The parts of a request context that tell whether the request ended. */
export interface RequestEnd {
  readonly signal?: AbortSignal;
  readonly deadlineAt?: number;
}

/**
 * Tells whether the request ended: its signal aborted (its client left or it
 * passed its timeout), or its deadline passed. A client call bound by the
 * deadline can stop a moment before the timeout fires: the request then ends
 * here, and its signal aborts at once, so the rest of its work stops too.
 */
export function requestEnded(request: RequestEnd | undefined): boolean {
  if (request?.signal?.aborted) return true;
  if (request?.deadlineAt === undefined || Date.now() < request.deadlineAt) return false;
  (request as { __endRequest?: () => void }).__endRequest?.();
  return true;
}

/**
 * Tells whether `error` is the stop of a request that ended: the reason of its
 * aborted signal, or an abort, timeout, query abort or canceled client call,
 * directly or as the `cause` of the error thrown. A client call that met its
 * deadline counts too when the request ended at its own deadline. Any other
 * error is a failure of its own, even when the request ended before it was thrown.
 */
export function isRequestStop(error: unknown, request: RequestEnd | undefined): boolean {
  if (!requestEnded(request)) return false;
  const reason: unknown = request?.signal?.aborted ? request.signal.reason : undefined;
  const timedOut =
    (request?.deadlineAt !== undefined && Date.now() >= request.deadlineAt) ||
    (reason as { name?: unknown } | undefined)?.name === 'TimeoutError';
  let current: unknown = error;
  for (let depth = 0; depth < 8 && current; depth++) {
    if (reason !== undefined && current === reason) return true;
    if (typeof current !== 'object') return false;
    const name = (current as { name?: unknown }).name;
    if (typeof name === 'string' && REQUEST_STOP_NAMES.has(name)) return true;
    if (timedOut && name === 'ClientDeadlineError') return true;
    current = (current as { cause?: unknown }).cause;
  }
  return false;
}

/**
 * Records `error` as the error of the request `context` serves. When the response
 * status is 500 or more, the request log line of `LoggerMiddleware` carries it.
 */
export function recordRequestError(context: HttpRequestContext, error: unknown): void {
  (context as HttpRequestContextInternal).__requestError = error;
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

/**
 * The request's response callbacks applied to `r`, read from the request's
 * own context; a callback that refuses the request's Accept leaves it
 * unanswered, as {@link skipNotAcceptable} does for a single candidate.
 */
function flushRequest(httpContext: HttpRequestContextInternal, r: HttpResponse | undefined): HttpResponse | undefined {
  const cbs = (httpContext as ContextWithCallback).__callbacks;
  if (!cbs || !(r instanceof HttpResponse)) return r;
  try {
    let rh = r;
    for (const cb of cbs) rh = cb(rh);
    return rh;
  } catch (e) {
    if (e instanceof NotAcceptableException) return undefined;
    throw e;
  }
}

/** Records `candidate` as the handler the request now tries. */
function useCandidate(
  httpContext: HttpRequestContextInternal,
  candidate: HandlerResult<RouteHandler<HttpRequestContext>>,
): void {
  httpContext.route = candidate.route;
  httpContext.params = candidate.params;
  httpContext.statusCode = candidate.statusCode;
  httpContext.__firstPartyErrors = candidate.firstPartyErrors;
}

/** A candidate that refused the request's Accept leaves it unanswered, as the next candidate would. */
function skipNotAcceptable(e: unknown): undefined {
  if (e instanceof NotAcceptableException) return undefined;
  throw e;
}

function isThenable(value: unknown): value is PromiseLike<unknown> {
  return (
    value instanceof Promise ||
    (typeof value === 'object' && value !== null && typeof (value as PromiseLike<unknown>).then === 'function')
  );
}
