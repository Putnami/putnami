import { useConfig } from '@putnami/runtime';
import { cookies } from './cookies';
import { SessionConfig } from './session.config';
import { registerSessionStore, type SessionStore } from './session.store';

/**
 * Cookie-based session store.
 *
 * Stores all session data in an encrypted cookie on the client.
 * The session ID parameter is ignored since data is tied to the cookie itself.
 *
 * **Characteristics:**
 * - Data is encrypted with authenticated AES-256-GCM (integrity-protected via auth tag)
 * - Stateless - no server-side storage needed
 * - Limited to ~4KB total (browser cookie limit)
 * - Good for small session data (user ID, roles, preferences)
 *
 * @example Configuration
 * ```yaml
 * session:
 *   store: 'cookie'
 *   cookieSecret: '...'  # Required
 * ```
 */
export class CookieSessionStore implements SessionStore {
  get<T>(sessionId: string, key: string): T | undefined {
    const session = this.getAll<Record<string, unknown>>(sessionId);
    return session?.[key] as T | undefined;
  }

  set<T>(sessionId: string, key: string, value: T): void {
    const session = this.getAll<Record<string, unknown>>(sessionId);
    session[key] = value;
    this.setAll(sessionId, session);
  }

  delete(sessionId: string, key: string): void {
    const session = this.getAll<Record<string, unknown>>(sessionId);
    if (session?.[key]) {
      delete session[key];
      this.setAll(sessionId, session);
    }
  }

  getAll<S = Record<string, unknown>>(_sessionId: string): S {
    const config = useConfig(SessionConfig);
    const store = cookies();
    const cookie = store.get(config.cookieName);
    if (!cookie) return {} as S;
    try {
      return JSON.parse(cookie) as S;
    } catch {
      store.warn('session:cookie', 'Failed to parse session cookie, resetting session');
      return {} as S;
    }
  }

  setAll<S>(_sessionId: string, session: S): void {
    const config = useConfig(SessionConfig);
    cookies().set(config.cookieName, JSON.stringify(session));
  }

  deleteAll(_sessionId: string): void {
    const config = useConfig(SessionConfig);
    cookies().set(config.cookieName, null);
  }

  exists(_sessionId: string): boolean {
    const config = useConfig(SessionConfig);
    return cookies().get(config.cookieName) !== undefined;
  }
}

// Auto-register on import
registerSessionStore('cookie', () => new CookieSessionStore());
