import { useRouteError as _useRouteError } from 'react-router';

/**
 * The shape React Router gives a thrown `Response` — a 4xx/5xx raised by a
 * loader, an action, or the router itself. Re-exported so an error boundary
 * can name the type without depending on `react-router`.
 */
export type { ErrorResponse } from 'react-router';

/**
 * Narrows an error-boundary value to a thrown `Response`, making `status`,
 * `statusText`, and `data` readable.
 *
 * Re-exported so an error boundary never has to import `react-router`
 * directly.
 *
 * @example
 * ```tsx
 * const err = useRouteError();
 * if (isRouteErrorResponse(err)) {
 *   return <div>{err.status} {err.statusText}</div>;
 * }
 * ```
 */
export { isRouteErrorResponse } from 'react-router';

/**
 * Returns the error caught by the nearest error boundary.
 *
 * Call it inside the component passed to `error().render(...)` or
 * `notFound().render(...)`. A route can throw any value, so the default return
 * type is `unknown`: either narrow it with `isRouteErrorResponse`, or name the
 * type you expect with the type parameter.
 *
 * @template E - The expected type of the thrown value. This is an unchecked
 * assertion, not a check: a route that throws a 4xx/5xx `Response` reaches the
 * boundary as an `ErrorResponse`, so `useRouteError<Error>()` would read
 * `undefined` from `.message`. Name a type only when the boundary cannot
 * receive anything else; otherwise narrow the default `unknown`.
 * @returns The thrown value, cast to type E
 *
 * @example
 * ```tsx
 * // src/app/error.tsx
 * import { error, isRouteErrorResponse, useRouteError } from '@putnami/web';
 *
 * export default error().render(function ErrorPage() {
 *   const err = useRouteError();
 *   if (isRouteErrorResponse(err)) {
 *     return <div>{err.status} — {err.statusText}</div>;
 *   }
 *   return <div>Error: {err instanceof Error ? err.message : 'Unknown error'}</div>;
 * });
 * ```
 */
export const useRouteError = <E = unknown>(): E => _useRouteError() as E;
