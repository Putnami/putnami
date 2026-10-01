import type { HttpRequestContext } from '@putnami/application';
import type { SqlClient } from '@putnami/database';
import {
  ArrayOf,
  Config,
  Default,
  type InferConfig,
  Int,
  OneOf,
  Optional,
  Sensitive,
  useRawConfigSection,
} from '@putnami/runtime';
import type { DeclaredEvents } from './declare';
import type { Sink } from './sink/sink';

/**
 * Analytics configuration (`analytics` section).
 *
 * Cookieless by default: the visitor identity is a daily-rotating server-side
 * HMAC, so no banner is required. `mode: 'identified'` writes a persistent
 * first-party cookie and requires an app-supplied consent callback.
 *
 * The five `flush*`/`queue*` keys size the asynchronous write queue: no
 * response ever waits for the database, and one request every
 * `flushIntervalMs` pays at most `flushWaitMs` to push a batch through. See
 * `doc/adr/0004-writes-are-asynchronous-and-lossy.md`.
 *
 * `schema` is unset by default: the tables then follow the schema another SQL
 * source, or `sql({ datasource })`, declares for the same datasource, and
 * land in `public` when nothing does.
 *
 * @example YAML configuration
 * ```yaml
 * analytics:
 *   enabled: true
 *   datasource: analytics
 *   secret: '...'      # any string >= 32 chars; falls back to session.cookieSecret
 * ```
 */
export const AnalyticsConfig = Config('analytics', {
  enabled: Default(Boolean, true),
  mode: Default(OneOf('cookieless', 'identified'), 'cookieless'),
  datasource: Default(String, 'analytics'),
  schema: Optional(String),
  secret: Sensitive(Optional(String)),
  serverPageViews: Default(Boolean, true),
  respectGpc: Default(Boolean, true),
  respectDnt: Default(Boolean, true),
  countryHeader: Optional(String),
  trustedProxies: Optional(ArrayOf(String)),
  retentionRawDays: Default(Int, 90),
  retentionAggregateDays: Default(Int, 760),
  retentionMode: Default(OneOf('sweep', 'pg_cron', 'off'), 'sweep'),
  maxPathKeysPerDay: Default(Int, 2000),
  rateLimitPerMinute: Default(Int, 120),
  flushIntervalMs: Default(Int, 5000),
  flushWaitMs: Default(Int, 1000),
  queueCapacity: Default(Int, 5000),
  flushBatch: Default(Int, 200),
  flushDeadlineMs: Default(Int, 5000),
  cookieName: Default(String, '_pa'),
  cookieMaxAgeDays: Default(Int, 390),
});

/** Resolved shape of {@link AnalyticsConfig}. */
export type AnalyticsConfigValues = InferConfig<typeof AnalyticsConfig>;

/**
 * Programmatic options passed to `analytics(options)`.
 *
 * Everything expressible in YAML lives in {@link AnalyticsConfig}; the two
 * members below are code, not configuration.
 */
export interface AnalyticsOptions extends Partial<AnalyticsConfigValues> {
  /** Consent gate, required when `mode === 'identified'`. */
  consent?: (ctx: HttpRequestContext) => boolean | Promise<boolean>;
  /** Declared action events and their property schemas. */
  events?: DeclaredEvents;
  /**
   * Replaces the sink, so a test can observe what would be written without a
   * database. Underscored and undocumented in the guides on purpose: it is a
   * seam for this package's own proofs, not a supported option.
   *
   * @internal
   */
  __sink?: Sink;
  /**
   * Replaces the pool the Postgres sink writes through, so a test can observe
   * the statements the plugin's own sink issues without a database. The same
   * kind of seam as `__sink`.
   *
   * @internal
   */
  __connect?: (name: string) => Promise<SqlClient>;
}

/** Message raised when no key is available for the visitor hash. */
export const MISSING_SECRET_MESSAGE =
  'analytics: set analytics.secret (any string ≥ 32 chars) or session.cookieSecret; the visitor hash needs a server-side key';

/**
 * Resolves the server-side key used by the visitor hash.
 *
 * Order: `analytics.secret`, then `session.cookieSecret` read as a raw section
 * so a missing session config does not throw. Fails closed when neither is set.
 *
 * @param config - The resolved analytics configuration.
 * @returns The secret to key the visitor HMAC with.
 * @throws Error when no secret is configured.
 */
export function resolveSecret(config: AnalyticsConfigValues): string {
  if (config.secret) {
    return config.secret;
  }
  const sessionSecret = useRawConfigSection('session')?.['cookieSecret'];
  if (typeof sessionSecret === 'string' && sessionSecret.length > 0) {
    return sessionSecret;
  }
  throw new Error(MISSING_SECRET_MESSAGE);
}
