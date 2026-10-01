import { useCsrfToken } from '../../shared/csrf-context';

/**
 * Renders a hidden `<input>` carrying the CSRF token.
 *
 * Use inside a plain `<form method="post">` so the token is submitted as a
 * body field and validated by the CSRF middleware — no JavaScript required.
 *
 * ```tsx
 * <form method="post" action="/contact">
 *   <CsrfInput />
 *   <input name="name" />
 *   <button type="submit">Send</button>
 * </form>
 * ```
 */
export function CsrfInput({ name = '_csrf' }: { name?: string }) {
  const token = useCsrfToken();
  if (!token) return null;
  return <input type='hidden' name={name} value={token} />;
}
