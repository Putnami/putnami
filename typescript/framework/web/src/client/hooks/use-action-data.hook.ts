import { useActionData as _useActionData } from 'react-router';

/**
 * Discriminated union for action responses.
 * Use `result.ok` to narrow the type — success returns your data,
 * failure returns error details.
 *
 * @example
 * ```tsx
 * const result = useActionData<{ user: User }>();
 * if (result?.ok) {
 *   // result.user is typed
 *   console.log(result.user.name);
 * } else if (result) {
 *   // result.error is typed
 *   console.log(result.error);
 * }
 * ```
 */
export type ActionResult<TSuccess = unknown> =
  | ({ ok: true; status: number } & TSuccess)
  | { ok: false; status: number; error?: string; errors?: Record<string, string[]> };

/**
 * Returns the action data for the current route, if an action has been performed.
 *
 * Returns a discriminated union: check `result.ok` to narrow between
 * success data (your return type) and error data.
 *
 * @template R - The expected return type on success
 * @returns The action result, or undefined if no action has been performed
 *
 * @example
 * ```tsx
 * function ContactForm() {
 *   const result = useActionData<{ message: string }>();
 *   if (result?.ok) {
 *     return <div>{result.message}</div>;
 *   }
 *   if (result && !result.ok) {
 *     return <div className="error">{result.error}</div>;
 *   }
 *   return <form>...</form>;
 * }
 * ```
 */
export const useActionData = <R = unknown>(): ActionResult<R> | undefined =>
  _useActionData() as ActionResult<R> | undefined;
