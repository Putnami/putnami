import { HttpException, useLogger } from '@putnami/runtime';
import type { HttpRequestContextInternal } from './http-context.type';
import type { HttpMiddleware } from './http-middleware.type';

/** Configuration for the logger middleware. */
export interface LoggerOptions {
  /** Route paths to exclude from logging (e.g. health-check endpoints). */
  exclude?: string[];
}

/** Creates a middleware that logs HTTP request method, route, status, and duration. */
export const LoggerMiddleware = (options: LoggerOptions = {}): HttpMiddleware => {
  const exclude = new Set(options.exclude || []);

  return async (context, next) => {
    const { method, route, path } = context;

    let routePath = route || path();
    if (!routePath.startsWith('/')) {
      routePath = `/${routePath}`;
    }

    // Check exclusion before timing to avoid unnecessary work
    if (exclude.has(routePath) || exclude.has(path())) {
      return next();
    }

    const logger = useLogger('http');
    logger.with('http', { method, routePath });
    const start = Date.now();

    try {
      const response = await next();
      const durationMs = Date.now() - start;
      // HttpResponse instances may omit `status` to mean implicit 200.
      const status = response ? (response.status ?? 200) : 404;
      const requestError = (context as HttpRequestContextInternal).__requestError;
      logger.with('http', { status, durationMs, outcome: status < 500 ? 'success' : 'failure' });

      if (status >= 500 && requestError) {
        // Message stays `[METHOD] routePath` on every path — severity and the
        // structured error (passed as the log param → entry.error) carry the
        // failure, so no error text leaks into the message.
        (context as HttpRequestContextInternal).__requestErrorLogged = true;
        logger.error(`[${method}] ${routePath}`, requestError);
      } else {
        logger.info(`[${method}] ${routePath}`);
      }
      return response;
    } catch (error) {
      // Errors that escape the dispatcher (framework-level failures)
      let status = 500;
      if (error instanceof HttpException) {
        status = error.getStatus();
      }
      const durationMs = Date.now() - start;
      logger.with('http', { status, durationMs, outcome: status < 500 ? 'success' : 'failure' });
      (context as HttpRequestContextInternal).__requestErrorLogged = true;
      logger.error(`[${method}] ${routePath}`, error);
      throw error;
    }
  };
};
