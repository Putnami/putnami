/**
 * Default timeout (ms) for client-side loader/action data fetches.
 *
 * Mirrors `PutnamiReactConfig.clientFetchTimeout`. The server serializes the
 * configured value to `window.__reactClientFetchTimeoutMs` during SSR (see
 * `page.renderer.ts`); the client reads it here and falls back to this default
 * when the global is absent (e.g. pure client-side navigation before hydration,
 * or when the value equals the default and was not emitted).
 */
export const DEFAULT_CLIENT_FETCH_TIMEOUT_MS = 30_000;

declare global {
  interface Window {
    __reactClientFetchTimeoutMs?: number;
  }
}

/** Resolve the client fetch timeout from the SSR-injected global, or the default. */
export function clientFetchTimeoutMs(): number {
  const injected = typeof window !== 'undefined' ? window.__reactClientFetchTimeoutMs : undefined;
  return typeof injected === 'number' && injected > 0 ? injected : DEFAULT_CLIENT_FETCH_TIMEOUT_MS;
}
