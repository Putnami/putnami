import { createContext, useContext } from 'react';

/**
 * React context for the CSRF token during SSR.
 * Populated by the page renderer from the request cookie.
 */
export const CsrfTokenContext = createContext<string | undefined>(undefined);

/**
 * Hook to access the CSRF token in a React component.
 * During SSR, reads from CsrfTokenContext; on the client, reads from document.cookie.
 */
export function useCsrfToken(): string | undefined {
  const ssrToken = useContext(CsrfTokenContext);
  if (ssrToken) return ssrToken;
  if (typeof document === 'undefined') return undefined;
  const match = document.cookie.split('; ').find((c) => c.startsWith('_csrf='));
  if (!match) return undefined;
  const eqIndex = match.indexOf('=');
  return eqIndex >= 0 ? match.slice(eqIndex + 1) : undefined;
}
