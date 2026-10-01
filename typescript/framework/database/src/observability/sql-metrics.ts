import { useLogger } from '@putnami/runtime';
import { incCounter, observeHistogram, setGauge } from '@putnami/application';
import { DATABASE_LOGGER, logQueryExecuted, logQueryFailed, logSlowQuery, logTransaction } from './database-logging';

/**
 * SQL operation types tracked by observability.
 */
export type SqlOperation = 'find' | 'count' | 'save' | 'saveMany' | 'delete' | 'deleteMany' | 'exists';

/**
 * Transaction-boundary outcome recorded by {@link recordTransaction}. The
 * transaction runner (`runInTransaction`/`UnitOfWork`) knows only whether the
 * transaction committed or rolled back; the richer `transaction.Outcome`
 * taxonomy is a repository-helper result not plumbed to the boundary. Mirrors
 * the Go adapter's `TxOutcome`.
 */
export type SqlTxOutcome = 'committed' | 'rolled-back';

/**
 * Structured metrics captured for each query execution.
 */
export interface QueryMetrics {
  operation: SqlOperation;
  table: string;
  /**
   * Duration in milliseconds. MAY be fractional: `Repository.observe` measures
   * with `performance.now()` so the log record can carry a real `durationUs`
   * alongside `durationMs` (sub-millisecond queries are the norm here). The
   * metric histograms take the value as-is; the log record rounds both units.
   */
  duration: number;
  /** Number of rows returned or affected. */
  rowCount?: number;
  /** Logical datasource the query ran on; defaults to `'default'` on the record. */
  datasource?: string;
}

// ---------------------------------------------------------------------------
// Query metrics
// ---------------------------------------------------------------------------

/**
 * Record a successful query with structured logging and telemetry metrics.
 *
 * Emits:
 * - Counter: `sql.{operation}.{table}` (query count)
 * - Histogram: `sql.{operation}.{table}.duration` (ms)
 * - Histogram: `sql.query.duration` (aggregate across all tables)
 *
 * Logs the contract's `query executed` record at `debug` (see
 * ./database-logging.ts, which owns every record shape of this boundary).
 */
export function recordQuery(metrics: QueryMetrics): void {
  const { operation, table, duration } = metrics;

  incCounter(`sql.${operation}.${table}`);
  observeHistogram(`sql.${operation}.${table}.duration`, duration);
  observeHistogram('sql.query.duration', duration);

  logQueryExecuted(metrics);
}

/**
 * Record a failed query with structured logging and telemetry metrics.
 *
 * Emits:
 * - Counter: `sql.{operation}.{table}.error`
 * - Counter: `sql.query.error` (aggregate)
 * - Histogram: `sql.query.duration` (even failures contribute to latency)
 *
 * Logs the contract's `query failed` record at **`warn`**, with the real error
 * value as a log param: the data layer reports outcome + latency and re-throws,
 * and the boundary that fails owns `error` severity exactly once. Passing the
 * error as a param (rather than an `{ error: '…' }` field) is what puts it in
 * `entry.error` — the JSON sink skips reserved keys, so a field form would be
 * silently dropped from the JSON output.
 */
export function recordQueryError(
  operation: SqlOperation,
  table: string,
  duration: number,
  error: unknown,
  datasource?: string,
): void {
  incCounter(`sql.${operation}.${table}.error`);
  incCounter('sql.query.error');
  observeHistogram('sql.query.duration', duration);

  logQueryFailed({ operation, table, duration, datasource }, error);
}

/**
 * Emit a warning when a query exceeds the configured slow-query threshold.
 *
 * Emits:
 * - Counter: `sql.query.slow`
 *
 * Logs the contract's `slow query` record at `warn` with the breached
 * `thresholdMs` as a field — the message stays a constant so log-based metrics
 * can match on it.
 */
export function recordSlowQuery(metrics: QueryMetrics, thresholdMs: number): void {
  if (metrics.duration >= thresholdMs) {
    incCounter('sql.query.slow');
    logSlowQuery(metrics, thresholdMs);
  }
}

// ---------------------------------------------------------------------------
// Transaction-boundary metrics
// ---------------------------------------------------------------------------

/**
 * Structured metrics captured at a transaction commit/rollback boundary.
 */
export interface TransactionMetrics {
  /** Logical datasource name; defaults to `'default'`. */
  datasource?: string;
  /** Whether the transaction committed or rolled back. */
  outcome: SqlTxOutcome;
  /** Transaction duration in milliseconds (BEGIN → commit/rollback). */
  duration: number;
  /** Retries the runner performed (always 0 today — no retry loop). */
  retries?: number;
  /**
   * The ALREADY-CLASSIFIED, secret-free rollback cause (a SQLSTATE, a
   * repository error code, an error class, or a sentinel). Only for a
   * `rolled-back` outcome. The caller MUST pass a classified code (see
   * `classifyRollbackCause`), never a raw error message — this function only
   * ever emits the value verbatim, so passing a message here would leak it.
   */
  cause?: string;
}

/**
 * Record a transaction-boundary measurement as spans (structured log) + metrics.
 *
 * Emits:
 * - Histogram: `sql.tx.duration` (ms)
 * - Histogram: `sql.tx.retries`
 * - Counter: `sql.tx.outcome.{committed|rolled-back}`
 * - Counter: `sql.tx.rollback.{cause}` (rolled-back only)
 *
 * Secret safety: the rollback cause is a classified code only. This function
 * never reads or emits an error message, a bound parameter value, or row data —
 * it is the load-bearing secret-safety invariant. Mirrors the Go adapter's `Pool.observeTx`
 * (`go/framework/database/metrics.go`) metric names byte-for-byte.
 */
export function recordTransaction(metrics: TransactionMetrics): void {
  const { outcome, duration } = metrics;
  const retries = metrics.retries ?? 0;
  const datasource = metrics.datasource ?? 'default';

  observeHistogram('sql.tx.duration', duration);
  observeHistogram('sql.tx.retries', retries);
  incCounter(`sql.tx.outcome.${outcome}`);

  const cause = outcome === 'rolled-back' ? metrics.cause || 'unknown' : undefined;
  if (cause) {
    incCounter(`sql.tx.rollback.${cause}`);
  }
  // The record carries the classified cause ONLY — never the raw error.
  logTransaction({ datasource, outcome, duration, retries, cause });
}

// ---------------------------------------------------------------------------
// Connection pool metrics
// ---------------------------------------------------------------------------

/**
 * Record that a new PHYSICAL connection pool was created — one per database
 * reached, not per datasource: datasources of one database share a pool (see
 * postgres/physical-pool.ts).
 *
 * Emits:
 * - Counter: `sql.pool.created`
 *
 * Logs at `debug` level. The counter is the operator-facing lifecycle signal.
 */
export function recordPoolCreated(name?: string): void {
  incCounter('sql.pool.created');
  useLogger(DATABASE_LOGGER).debug('Database pool created', { datasource: name ?? 'default' });
}

/**
 * Record that a PHYSICAL connection pool was closed — its last datasource
 * released it.
 *
 * Emits:
 * - Counter: `sql.pool.closed`
 *
 * Logs at `debug` level. The counter is the operator-facing lifecycle signal.
 */
export function recordPoolClosed(name?: string): void {
  incCounter('sql.pool.closed');
  useLogger(DATABASE_LOGGER).debug('Database pool closed', { datasource: name ?? 'default' });
}

/**
 * Set the current number of open PHYSICAL connection pools (databases reached,
 * not datasources opened).
 *
 * Emits:
 * - Gauge: `sql.pool.count`
 */
export function recordPoolCount(count: number): void {
  setGauge('sql.pool.count', count);
}

/**
 * Record the configured maximum pool size.
 *
 * Emits:
 * - Gauge: `sql.pool.max`
 */
export function recordPoolSize(max: number): void {
  setGauge('sql.pool.max', max);
}

/**
 * Connection pool health snapshot from pg_stat_activity.
 */
export interface PoolHealthStats {
  /** Total open connections (active + idle) */
  total: number;
  /** Connections currently executing a query */
  active: number;
  /** Connections waiting for work */
  idle: number;
  /** Connections idle inside a transaction */
  idleInTransaction: number;
  /** Maximum allowed connections (pool size) */
  max: number;
}

/**
 * Record a pool health snapshot as gauge metrics.
 *
 * Emits:
 * - Gauge: `sql.pool.connections.total`
 * - Gauge: `sql.pool.connections.active`
 * - Gauge: `sql.pool.connections.idle`
 * - Gauge: `sql.pool.connections.idle_in_transaction`
 * - Gauge: `sql.pool.utilization` (active / max as percentage 0-100)
 */
export function recordPoolHealth(stats: PoolHealthStats): void {
  setGauge('sql.pool.connections.total', stats.total);
  setGauge('sql.pool.connections.active', stats.active);
  setGauge('sql.pool.connections.idle', stats.idle);
  setGauge('sql.pool.connections.idle_in_transaction', stats.idleInTransaction);
  if (stats.max > 0) {
    setGauge('sql.pool.utilization', Math.round((stats.active / stats.max) * 100));
  }
}
