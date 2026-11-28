import {
  type HttpRequestContext,
  HttpResponse,
  type RouteHandler,
  recordRequestError,
  requestEnded,
} from '@putnami/application';
import { getHttpStatusText, HttpException } from '@putnami/runtime';

/** What a failed loader shows the browser: the `data` of its route error response. */
export interface LoaderErrorData {
  statusCode: number;
  error: string;
  message: string;
}

/**
 * Projects a loader `HttpException` onto what reaches the browser, on a full
 * page load (the hydration payload) and on a client navigation (the loader
 * endpoint's body) alike: its status, the status text of the status table
 * (the exception's name for a status outside it), and its message. A response
 * built on an `Error` answers the status text instead: the cause's message,
 * which `HttpException` takes as its own, may carry a driver's detail. The
 * rest of `getResponse()` stays on the server.
 *
 * @param exception - The exception the loader threw, with a status of 400 or more.
 * @returns The projection, the same on both paths.
 */
export function loaderErrorData(exception: HttpException): LoaderErrorData {
  const status = exception.getStatus();
  const statusText = getHttpStatusText(status) ?? exception.name;
  const message = exception.getResponse() instanceof Error ? statusText : exception.message;
  return { statusCode: status, error: statusText, message };
}

/**
 * Answers an `HttpException` of 400 or more that a page or layout loader
 * endpoint throws, its route middleware included, with {@link loaderErrorData}
 * and its status, as a full page load gives it to the error boundary. The
 * exception is recorded as the request error and logged at no ERROR, as on a
 * full page load. A request that ended, any other error and a status below 400
 * go on to the HTTP dispatcher unchanged. API `endpoint()` routes do not use it.
 *
 * @param handler - The loader endpoint handler, middleware included.
 * @returns The same handler, with its `HttpException` answered as its projection.
 */
export function projectedLoaderErrors(handler: RouteHandler): RouteHandler {
  return async (ctx: HttpRequestContext) => {
    try {
      return await handler(ctx);
    } catch (error) {
      if (!(error instanceof HttpException) || error.getStatus() < 400 || requestEnded(ctx)) throw error;
      recordRequestError(ctx, error);
      return HttpResponse.json(loaderErrorData(error), { status: error.getStatus() });
    }
  };
}
