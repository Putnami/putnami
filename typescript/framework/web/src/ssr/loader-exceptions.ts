import { getHttpStatusText, HttpException } from '@putnami/runtime';
import type { StaticHandlerContext } from 'react-router';
import { isRouteErrorResponse, UNSAFE_ErrorResponseImpl } from 'react-router';

/** Whether a loader error is a 4xx `HttpException`: an answer the loader chose, not a failure. */
export function isClientErrorException(error: unknown): boolean {
  if (!(error instanceof HttpException)) return false;
  const status = error.getStatus();
  return status >= 400 && status < 500;
}

/**
 * Answers a 4xx or 5xx `HttpException` a loader threw with its own status:
 * React Router keeps it as an unknown error and computes a 500, so each one is
 * replaced by the route error response of its status, which the not-found and
 * error boundaries render, with `{ statusCode, error, message }` as its
 * `data`, and the router status follows the first error as React Router
 * computes it. Live SSR runs it after it records the exception itself as the
 * request error; a static render runs it on the build-time query.
 */
export function answerLoaderExceptions(context: StaticHandlerContext | Response): void {
  if (context instanceof Response || !context.errors) return;
  let converted = false;
  const errors: Record<string, unknown> = {};
  for (const [routeId, error] of Object.entries(context.errors)) {
    // An exception with a status below 400 is not an error answer.
    if (error instanceof HttpException && error.getStatus() >= 400) {
      const status = error.getStatus();
      const statusText = getHttpStatusText(status) ?? error.name;
      // Only the status and the message reach the hydration payload, as
      // `sanitizeErrors` keeps of an `Error`: the exception's details stay on
      // the server.
      errors[routeId] = new UNSAFE_ErrorResponseImpl(status, statusText, {
        statusCode: status,
        error: statusText,
        message: error.message,
      });
      converted = true;
    } else {
      errors[routeId] = error;
    }
  }
  if (!converted) return;
  context.errors = errors;
  const first = Object.values(errors)[0];
  context.statusCode = isRouteErrorResponse(first) ? first.status : 500;
}
