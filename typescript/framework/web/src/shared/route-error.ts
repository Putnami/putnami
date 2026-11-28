import { isRouteErrorResponse } from 'react-router';

/**
 * The message and stack a default error boundary shows in development, for an
 * `Error`, a route error response, or the `{ message, status }` that
 * `sanitizeErrors` keeps of an `Error` for hydration.
 */
export function describeRouteError(error: unknown): { message: string; stack?: string } {
  if (isRouteErrorResponse(error)) {
    const data = error.data as { message?: unknown } | null | undefined;
    const message = typeof data?.message === 'string' ? data.message : undefined;
    return { message: `${error.status} ${error.statusText}${message ? `: ${message}` : ''}` };
  }
  if (error instanceof Error) return { message: error.message, stack: error.stack };
  const message = (error as { message?: unknown } | null | undefined)?.message;
  return { message: typeof message === 'string' ? message : String(error) };
}
