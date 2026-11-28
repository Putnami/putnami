import { isRouteErrorResponse, useRouteError } from 'react-router';
import { shouldExposeClientErrors } from './expose-errors';

/** The message and stack a default boundary shows in development, for an `Error` or a route error response. */
function describeRouteError(error: unknown): { message: string; stack?: string } {
  if (isRouteErrorResponse(error)) {
    const data = error.data as { message?: unknown } | null | undefined;
    const message = typeof data?.message === 'string' ? data.message : undefined;
    return { message: `${error.status} ${error.statusText}${message ? `: ${message}` : ''}` };
  }
  if (error instanceof Error) return { message: error.message, stack: error.stack };
  // A hydrated error arrives as the `{ message, status }` `sanitizeErrors` kept of it.
  const message = (error as { message?: unknown } | null | undefined)?.message;
  return { message: typeof message === 'string' ? message : String(error) };
}

export function ErrorBoundary() {
  const error = describeRouteError(useRouteError());
  // Take the decision at render time, not at bundle time. Written inline,
  // `process.env.NODE_ENV !== 'production'` is constant-folded to `true` when
  // this package is published, which froze the guard open in every consumer
  // build and leaked `error.stack` in production. This boundary runs in the
  // browser too, where there is no environment to read at all, so the answer
  // comes from the flag SSR injected — absent means "do not expose".
  const isDev = shouldExposeClientErrors();

  return (
    <>
      <h2>Oups, we met an issue!</h2>
      <div>
        <h3>{isDev ? error.message : 'An unexpected error occurred'}</h3>
        {isDev && error.stack && <pre>{error.stack}</pre>}
      </div>
    </>
  );
}
