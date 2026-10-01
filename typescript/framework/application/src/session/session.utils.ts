import { useConfig, useContext } from '@putnami/runtime';
import { cookies } from './cookies';
import { SessionConfig } from './session.config';
import { getRegisteredStore, type SessionStore } from './session.store';

// Import stores to trigger auto-registration
import './cookie.store';
import './memory.store';

type SessionContext = {
  sessionId?: string;
};

/**
 * Get the configured session store instance.
 *
 * Returns the store based on `session.store` config value.
 * Instances are cached as singletons.
 *
 * @returns The active session store
 *
 * @example
 * ```typescript
 * const store = useSessionStore();
 * store.set(sessionId, 'key', 'value');
 * ```
 */
export function useSessionStore(): SessionStore {
  const config = useConfig(SessionConfig);
  return getRegisteredStore(config.store);
}

/**
 * Get or generate the current session ID.
 *
 * - For cookie store: returns a fixed placeholder (data is in the cookie itself)
 * - For server-side stores: retrieves from cookie or generates a new UUID
 *
 * @returns The current session ID
 */
export function useSessionId(): string {
  const config = useConfig(SessionConfig);

  if (config.store === 'cookie') {
    // Cookie store doesn't use session IDs - data is in the cookie
    return '__cookie__';
  }

  // Server-side stores: get session ID from cookie or generate new one
  const context = useContext<SessionContext>();
  if (context.sessionId) {
    return context.sessionId;
  }

  const sessionIdCookie = cookies().get(`${config.cookieName}_id`);
  if (sessionIdCookie) {
    context.sessionId = sessionIdCookie;
    return sessionIdCookie;
  }

  // Generate new session ID
  const newSessionId = crypto.randomUUID();
  cookies().set(`${config.cookieName}_id`, newSessionId);
  context.sessionId = newSessionId;
  return newSessionId;
}

/**
 * Get all session data.
 *
 * @returns All session data as an object
 *
 * @example
 * ```typescript
 * interface MySession {
 *   userId: number;
 *   preferences: { theme: string };
 * }
 *
 * const session = useSessionAll<MySession>();
 * console.log(session.userId);
 * ```
 */
export function useSessionAll<S = Record<string, unknown>>(): S {
  const store = useSessionStore();
  const sessionId = useSessionId();
  return store.getAll<S>(sessionId);
}

/**
 * Delete all session data.
 *
 * Clears both the session data and the session ID cookie
 * (for server-side stores).
 *
 * @example
 * ```typescript
 * // Clear entire session on logout
 * deleteSessionAll();
 * ```
 */
export function deleteSessionAll(): void {
  const store = useSessionStore();
  const sessionId = useSessionId();
  store.deleteAll(sessionId);

  // Also clear session ID cookie for server-side stores
  const config = useConfig(SessionConfig);
  if (config.store !== 'cookie') {
    cookies().set(`${config.cookieName}_id`, null);
  }
}

/**
 * Delete a specific key from the session.
 *
 * @param key - Key to delete
 *
 * @example
 * ```typescript
 * deleteSession('temporaryToken');
 * ```
 */
export function deleteSession(key: string): void {
  const store = useSessionStore();
  const sessionId = useSessionId();
  store.delete(sessionId, key);
}

/**
 * Get a specific value from the session.
 *
 * @param key - Key to retrieve
 * @returns The value or undefined if not found
 *
 * @example
 * ```typescript
 * const userId = useSession<number>('userId');
 * const user = useSession<User>('user');
 * ```
 */
export function useSession<T>(key: string): T | undefined {
  const store = useSessionStore();
  const sessionId = useSessionId();
  return store.get<T>(sessionId, key);
}

/**
 * Set a specific value in the session.
 *
 * @param key - Key to store
 * @param value - Value to store (must be JSON-serializable)
 *
 * @example
 * ```typescript
 * setSession('userId', 123);
 * setSession('preferences', { theme: 'dark', language: 'en' });
 * ```
 */
export function setSession<T>(key: string, value: T): void {
  const store = useSessionStore();
  const sessionId = useSessionId();
  store.set(sessionId, key, value);
}

/**
 * Replace all session data.
 *
 * Overwrites the entire session with new data.
 * Use with caution - prefer `setSession` for individual keys.
 *
 * @param session - New session data
 *
 * @example
 * ```typescript
 * setSessionAll({ userId: 123, role: 'admin' });
 * ```
 */
export function setSessionAll<T>(session: T): void {
  const store = useSessionStore();
  const sessionId = useSessionId();
  store.setAll(sessionId, session);
}
