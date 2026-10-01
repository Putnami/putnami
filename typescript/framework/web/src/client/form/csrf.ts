const CSRF_COOKIE = '_csrf';

export function getCsrfToken(): string | undefined {
  if (typeof document === 'undefined') return undefined;
  const match = document.cookie.split('; ').find((c) => c.startsWith(`${CSRF_COOKIE}=`));
  if (!match) return undefined;
  const eqIndex = match.indexOf('=');
  return eqIndex >= 0 ? match.slice(eqIndex + 1) : undefined;
}
