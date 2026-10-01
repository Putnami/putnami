import { Config, Default, Int, OneOf } from '@putnami/runtime';

/**
 * Session store type options.
 * - `'cookie'` - Encrypted cookie-based storage (default)
 * - `'memory'` - Server-side in-memory storage with TTL
 * - `'database'` - PostgreSQL storage (requires @putnami/database)
 */
export type SessionStoreType = 'cookie' | 'memory' | 'database';

/**
 * Session configuration.
 *
 * @example YAML configuration
 * ```yaml
 * session:
 *   store: 'cookie'
 *   cookieName: 'session'
 *   cookieSecret: '...' # crypto.randomBytes(32).toString('hex')
 *   ttl: 604800         # 1 week in seconds
 * ```
 */
export const SessionConfig = Config('session', {
  store: Default(String, 'cookie'),
  cookieName: Default(String, 'session'),
  sameSite: Default(OneOf('lax', 'strict', 'none'), 'lax'),
  secure: Default(Boolean, true),
  algorithm: Default(String, 'aes-256-gcm'),
  cookieSecret: String,
  ttl: Default(Int, 60 * 60 * 24 * 7),
  /** Maximum number of in-memory sessions before oldest are evicted. Only applies to memory store. */
  maxSessions: Default(Int, 10_000),
});
