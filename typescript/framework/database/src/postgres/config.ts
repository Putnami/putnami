import { Config, Default, Int, Optional } from '@putnami/runtime';

/**
 * Database connectivity configuration.
 *
 * `host` accepts both a regular TCP hostname (e.g. `localhost`) and a Unix
 * socket directory (e.g. `/cloudsql/<project>:<region>:<instance>`). When
 * `host` starts with `/`, postgres.js connects through the socket and `port`
 * is ignored.
 *
 * `user` is optional: when omitted, the active GCP IAM principal is
 * resolved at startup — the metadata server's service-account email on
 * Cloud Run / GCE / Cloud Build, or `gcloud config get-value account` on
 * a dev laptop.
 *
 * `password` is optional: when omitted on a Unix socket host, an IAM
 * access token from the metadata server is used per-connection. On a
 * regular TCP host, leaving it unset is fine when the local
 * `cloud-sql-proxy` (or any other auth-managing proxy) handles login.
 */
export const PostgresConfig = Config('database', {
  host: Default(String, 'localhost'),
  port: Default(Int, 5432),
  database: String,
  user: Optional(String),
  password: Optional(String),
  // SSL defaults on for remote TCP hosts and off for unix-socket / loopback
  // hosts (see resolveSsl in options.ts). Set explicitly to force on or off.
  ssl: Optional(Boolean),
  poolSize: Default(Int, 10),
  debug: Default(Boolean, false),
  /**
   * Hard upper bound on the number of rows `find()` will return. A caller's
   * explicit `limit` is clamped to this value, so request-controlled input can
   * never request an unbounded result set. The implicit page size when no
   * `limit` is given stays at 1000.
   */
  maxRowLimit: Default(Int, 10_000),
  queryProfiling: Optional(Boolean),
  /** Queries slower than this threshold (ms) are logged at warn level. 0 disables. */
  slowQueryThresholdMs: Default(Int, 0),
  /** PostgreSQL statement_timeout in ms. Aborts any statement exceeding this duration. 0 disables. */
  statementTimeoutMs: Default(Int, 30_000),
  /** PostgreSQL idle_in_transaction_session_timeout in ms. Terminates idle transactions. 0 disables. */
  idleInTransactionTimeoutMs: Default(Int, 300_000),
  /** Connection establishment timeout in seconds. */
  connectTimeout: Default(Int, 10),
  /** Close idle connections after this many seconds. 0 disables. */
  idleTimeout: Default(Int, 60),
  /** Maximum connection lifetime in seconds. 0 uses driver default (random 45–90 min). */
  maxLifetime: Default(Int, 0),
});
