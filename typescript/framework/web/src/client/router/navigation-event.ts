/**
 * The DOM navigation seam.
 *
 * The browser router dispatches one `putnami:navigation` event per client-side
 * route change so any plugin can observe navigation without patching the
 * router. This module is browser-only by construction: it imports nothing, so
 * it can never pull a server-only API into the published browser graph.
 */

/** Name of the DOM event the browser router dispatches on client navigation. */
export const PUTNAMI_NAVIGATION_EVENT = 'putnami:navigation';

/** Payload carried by {@link PUTNAMI_NAVIGATION_EVENT}. */
export interface NavigationDetail {
  pathname: string;
  search: string;
  hash: string;
  /** Matched leaf route pattern in file-route form (`/tasks/[id]`), or `__unknown__`. */
  route: string;
  /** Previous pathname, or undefined for the first dispatch. */
  previous: string | undefined;
}

/**
 * Subscribes to client navigations. Returns the unsubscribe function. Outside a
 * browser it subscribes to nothing and the returned function is a no-op, so
 * callers never need a `typeof window` guard of their own.
 */
export function onNavigation(listener: (detail: NavigationDetail) => void): () => void {
  if (typeof window === 'undefined') return () => {};
  const handler = (event: Event) => listener((event as CustomEvent<NavigationDetail>).detail);
  window.addEventListener(PUTNAMI_NAVIGATION_EVENT, handler);
  return () => window.removeEventListener(PUTNAMI_NAVIGATION_EVENT, handler);
}

/** Dispatches a client navigation. A no-op when there is no DOM to dispatch on. */
export function dispatchNavigation(detail: NavigationDetail): void {
  if (typeof window === 'undefined' || typeof window.dispatchEvent !== 'function') return;
  window.dispatchEvent(new CustomEvent(PUTNAMI_NAVIGATION_EVENT, { detail }));
}
