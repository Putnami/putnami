import { useConfig } from '@putnami/runtime';
import { SessionConfig } from './session.config';
import { registerSessionStore, type SessionStore } from './session.store';

interface SessionEntry {
  data: Record<string, unknown>;
  expiresAt: number;
}

/**
 * In-memory session store.
 *
 * Stores session data in a server-side Map with TTL-based expiration.
 * Session ID is stored in an encrypted cookie, actual data stays on the server.
 *
 * **Characteristics:**
 * - Fast - no I/O overhead
 * - Automatic TTL expiration with periodic cleanup (every 5 minutes)
 * - Data lost on server restart
 * - Not suitable for multi-instance deployments (use database store instead)
 *
 * @example Configuration
 * ```yaml
 * session:
 *   store: 'memory'
 *   ttl: 604800  # 1 week
 * ```
 */
export class MemorySessionStore implements SessionStore {
  private readonly sessions = new Map<string, SessionEntry>();
  private cleanupInterval?: ReturnType<typeof setInterval>;

  constructor() {
    // Run cleanup every 5 minutes
    this.cleanupInterval = setInterval(() => this.cleanup(), 5 * 60 * 1000);
  }

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
    const entry = this.getEntry(sessionId);
    if (entry?.data?.[key]) {
      delete entry.data[key];
      this.touch(sessionId, entry);
    }
  }

  getAll<S = Record<string, unknown>>(sessionId: string): S {
    const entry = this.getEntry(sessionId);
    return (entry?.data ?? {}) as S;
  }

  setAll<S>(sessionId: string, session: S): void {
    const config = useConfig(SessionConfig);
    const expiresAt = Date.now() + config.ttl * 1000;
    this.sessions.set(sessionId, {
      data: session as Record<string, unknown>,
      expiresAt,
    });
    this.evictIfNeeded(config.maxSessions);
  }

  deleteAll(sessionId: string): void {
    this.sessions.delete(sessionId);
  }

  exists(sessionId: string): boolean {
    return this.getEntry(sessionId) !== undefined;
  }

  /** Stop the cleanup interval (for graceful shutdown) */
  dispose(): void {
    if (this.cleanupInterval) {
      clearInterval(this.cleanupInterval);
      this.cleanupInterval = undefined;
    }
  }

  private getEntry(sessionId: string): SessionEntry | undefined {
    const entry = this.sessions.get(sessionId);
    if (!entry) {
      return undefined;
    }

    // Check if expired
    if (Date.now() > entry.expiresAt) {
      this.sessions.delete(sessionId);
      return undefined;
    }

    return entry;
  }

  private touch(sessionId: string, entry: SessionEntry): void {
    const config = useConfig(SessionConfig);
    entry.expiresAt = Date.now() + config.ttl * 1000;
    this.sessions.set(sessionId, entry);
  }

  private evictIfNeeded(maxSessions: number): void {
    if (maxSessions <= 0 || this.sessions.size <= maxSessions) return;
    const entries = [...this.sessions.entries()].sort((a, b) => a[1].expiresAt - b[1].expiresAt);
    const toRemove = this.sessions.size - maxSessions;
    for (let i = 0; i < toRemove; i++) {
      this.sessions.delete(entries[i][0]);
    }
  }

  private cleanup(): void {
    const now = Date.now();
    for (const [sessionId, entry] of this.sessions.entries()) {
      if (now > entry.expiresAt) {
        this.sessions.delete(sessionId);
      }
    }
  }
}

// Auto-register on import
registerSessionStore('memory', () => new MemorySessionStore());
