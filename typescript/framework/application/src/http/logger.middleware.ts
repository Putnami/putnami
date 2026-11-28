import { HttpException, type Logger, useLogger } from '@putnami/runtime';
import { deferHttpLogContext } from './http-context.builder';
import type { HttpRequestContextInternal } from './http-context.type';
import type { HttpMiddleware } from './http-middleware.type';
import type { HttpResponse } from './http-response';
import { withCompiledForm } from './route-scope';

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

  /** The route path a request logs under, or `undefined` when it is excluded. */
  const loggedRoute = (context: HttpRequestContextInternal): string | undefined => {
    let routePath = context.route || context.path();
    if (!routePath.startsWith('/')) {
      routePath = `/${routePath}`;
    }
    if (exclude.size > 0 && (exclude.has(routePath) || exclude.has(context.path()))) return undefined;
    return routePath;
  };

  /** The access record of a request that answered `response`. */
  const logAnswer = (
    context: HttpRequestContextInternal,
    routePath: string,
    start: number,
    response: HttpResponse | undefined,
  ): void => {
    const status = response ? (response.status ?? 200) : 404;
    const requestError = context.__requestError;
    if (status >= 500 && requestError) {
      logError(context, routePath, status, start, requestError);
      return;
    }
    const logger = httpLogger();
    if (logger.isEnabled('debug')) {
      logger.with('http', { status, durationMs: Date.now() - start, outcome: status < 500 ? 'success' : 'failure' });
      logger.debug(`[${context.method}] ${routePath}`);
    } else {
      logger.debug();
    }
  };

  type Logged = HttpRequestContextInternal & { __accessRoute?: string; __accessStart?: number };

  const compiled = {
    enter: (ctx: HttpRequestContextInternal): undefined => {
      const context = ctx as Logged;
      const routePath = loggedRoute(context);
      context.__accessRoute = routePath;
      if (routePath === undefined) return undefined;
      if (!deferHttpLogContext(context, context.method, routePath)) {
        httpLogger().with('http', { method: context.method, routePath });
      }
      context.__accessStart = Date.now();
      return undefined;
    },
    leave: (ctx: HttpRequestContextInternal, response: HttpResponse | undefined) => {
      const context = ctx as Logged;
      if (context.__accessRoute !== undefined) logAnswer(context, context.__accessRoute, context.__accessStart as number, response);
      return response;
    },
    fail: (ctx: HttpRequestContextInternal, error: unknown) => {
      const context = ctx as Logged;
      if (context.__accessRoute === undefined) return;
      const status = error instanceof HttpException ? error.getStatus() : 500;
      logError(context, context.__accessRoute, status, context.__accessStart as number, error);
    },
  };

  const middleware: HttpMiddleware = async (context, next) => {
    const method = context.method;
    let routePath = context.route || context.path();
    if (!routePath.startsWith('/')) {
      routePath = `/${routePath}`;
    }

    // Check exclusion before timing to avoid unnecessary work
    if (exclude.size > 0 && (exclude.has(routePath) || exclude.has(context.path()))) {
      return next();
    }

    // The `{ method, routePath }` field every record of the request carries
    // is built when the first record is written: a request that writes none
    // never builds it.
    if (!deferHttpLogContext(context, method, routePath)) {
      httpLogger().with('http', { method, routePath });
    }
    const start = Date.now();

    let response: Awaited<ReturnType<typeof next>>;
    try {
      response = await next();
    } catch (error) {
      // Errors that escape the dispatcher (framework-level failures)
      logError(context, routePath, error instanceof HttpException ? error.getStatus() : 500, start, error);
      throw error;
    }

    // HttpResponse instances may omit `status` to mean implicit 200.
    const status = response ? (response.status ?? 200) : 404;
    const requestError = (context as HttpRequestContextInternal).__requestError;
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
  };
  return withCompiledForm(middleware, compiled as never);
};
