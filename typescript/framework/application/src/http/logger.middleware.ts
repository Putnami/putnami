import { HttpException, type Logger, useLogger } from '@putnami/runtime';
import { deferHttpLogContext } from './http-context.builder';
import type { HttpRequestContext, HttpRequestContextInternal } from './http-context.type';
import type { HttpMiddleware } from './http-middleware.type';
import { middlewareFromForm } from './middleware-form';

/** Configuration for the logger middleware. */
export interface LoggerOptions {
  /** Route paths to exclude from logging (e.g. health-check endpoints). */
  exclude?: string[];
}

/** Creates a middleware that logs HTTP request method, route, status, and duration. */
export const LoggerMiddleware = (options: LoggerOptions = {}): HttpMiddleware => {
  const exclude = new Set(options.exclude || []);
  // The `http` logger derived from the last base logger: a request served by
  // the same base reuses it instead of deriving a new one.
  let base: Logger | undefined;
  let named: Logger | undefined;
  const httpLogger = (): Logger => {
    const current = useLogger();
    if (current !== base || !named) {
      base = current;
      named = current.named('http');
    }
    return named;
  };

  /** Completes the request's `http` log field and writes its error record. */
  const logError = (
    context: HttpRequestContextInternal,
    routePath: string,
    status: number,
    start: number,
    error: unknown,
  ): void => {
    const logger = httpLogger();
    logger.with('http', { status, durationMs: Date.now() - start, outcome: status < 500 ? 'success' : 'failure' });
    context.__requestErrorLogged = true;
    // Message stays `[METHOD] routePath` on every path — severity and the
    // structured error (passed as the log param → entry.error) carry the
    // failure, so no error text leaks into the message.
    logger.error(`[${context.method}] ${routePath}`, error);
  };

  /** The path a request logs under: its route pattern, else its path. */
  const loggedRoute = (context: HttpRequestContext): string => {
    const routePath = context.route || context.path();
    return routePath.startsWith('/') ? routePath : `/${routePath}`;
  };

  return middlewareFromForm<LoggedRequest>({
    passes:
      exclude.size > 0 ? (context) => exclude.has(loggedRoute(context)) || exclude.has(context.path()) : undefined,
    enter: (context) => {
      const method = context.method;
      // Read before the inner chain: a HEAD request is matched to its GET route inside it.
      const routePath = loggedRoute(context);
      if (!deferHttpLogContext(context, method, routePath)) {
        httpLogger().with('http', { method, routePath });
      }
      return { method, routePath, start: Date.now() };
    },
    // What a request that answered leaves: an error record for a 5xx it failed with, else the access record.
    leave: (ctx, response, { method, routePath, start }) => {
      const context = ctx as HttpRequestContextInternal;
      // HttpResponse instances may omit `status` to mean implicit 200.
      const status = response ? (response.status ?? 200) : 404;
      const requestError = context.__requestError;
      if (status >= 500 && requestError) {
        logError(context, routePath, status, start, requestError);
        return response;
      }

      // The http.server.* metrics count every served request, so the access
      // line stays out of info (ADR 0002 of go.putnami.dev/logger).
      const logger = httpLogger();
      if (logger.isEnabled('debug')) {
        logger.with('http', { status, durationMs: Date.now() - start, outcome: status < 500 ? 'success' : 'failure' });
        logger.debug(`[${method}] ${routePath}`);
      } else {
        // Below debug the record is dropped: it is still counted as dropped.
        logger.debug();
      }
      return response;
    },
    // Errors that escape the dispatcher (framework-level failures).
    fail: (context, error, { routePath, start }) => {
      logError(
        context as HttpRequestContextInternal,
        routePath,
        error instanceof HttpException ? error.getStatus() : 500,
        start,
        error,
      );
    },
  });
};

/** What the logger keeps of a request between its start and its answer. */
interface LoggedRequest {
  readonly method: string;
  readonly routePath: string;
  readonly start: number;
}
