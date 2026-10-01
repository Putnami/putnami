import { DEFAULT_DATASOURCE } from '@putnami/migration';
import { useLogger } from '@putnami/runtime';

// ---------------------------------------------------------------------------
// Database boundary log contract — the TypeScript twin of Go's
// `go/framework/database/database_logging.go`.
//
// Every record shape of the database boundary (pool queries, transactions, and
// migrations) is built here exactly once, so a call site can neither invent a
// message, a severity, nor a field name. The contract itself is normative in
// `protocols/logging/conformance` (manifest + README); both runtimes must emit
// schema-equivalent records for the same boundary event.
// ---------------------------------------------------------------------------

/**
 * Pinned logger names of the contract ("Pinned logger names": dots, never
 * colons). `database` owns the query and transaction records; `database.migration`
 * owns every migration record (migrator, SQL runner, and — in
 * `@putnami/migration` — the registry).
 */
export const DATABASE_LOGGER = 'database';
export const DATABASE_MIGRATION_LOGGER = 'database.migration';

/**
 * Outcome vocabulary. Database records only ever report success or failure; a
 * rolled-back transaction reports failure and carries the CLASSIFIED reason in
 * `rollbackCause` — never raw error text.
 */
export const OUTCOME_SUCCESS = 'success';
export const OUTCOME_FAILURE = 'failure';

/**
 * Stable record messages. They are constants because log-based metrics and
 * alerts match on them: no identifier, duration, threshold, or error text is
 * ever interpolated into a message.
 */
const MSG_QUERY_EXECUTED = 'query executed';
const MSG_QUERY_FAILED = 'query failed';
const MSG_SLOW_QUERY = 'slow query';
const MSG_TX_COMMITTED = 'transaction committed';
const MSG_TX_ROLLED_BACK = 'transaction rolled back';
const MSG_MIGRATION_APPLIED = 'migration applied';
const MSG_MIGRATION_FAILED = 'migration failed';

/** The fields of one query record, as the timing call site measured them. */
interface QueryRecord {
  /** Statement kind (the runtime-specific `operation` vocabulary). */
  operation: string;
  table: string;
  /** Logical datasource; defaults to the canonical default when absent. */
  datasource?: string;
  /**
   * Wall duration in milliseconds. MAY be fractional: the call sites measure
   * with `performance.now()` so the record can carry a real `durationUs` —
   * sub-millisecond queries are the norm at this boundary.
   */
  duration: number;
  rowCount?: number;
}

/** Transaction-boundary outcome as the runner knows it (mirrors Go's `TxOutcome`). */
type TxRecordOutcome = 'committed' | 'rolled-back';

/** The fields of one transaction-boundary record. */
interface TransactionRecord {
  datasource?: string;
  outcome: TxRecordOutcome;
  /** Wall duration in milliseconds; may be fractional (see {@link QueryRecord.duration}). */
  duration: number;
  retries?: number;
  /** The ALREADY-CLASSIFIED, secret-free rollback cause; rolled back only. */
  cause?: string;
}

/**
 * The integer duration pair the contract requires on every `database` record.
 * `durationMs` alone loses every sub-millisecond query, which is why database
 * records carry `durationUs` too; both are derived from ONE measurement so they
 * can never disagree.
 */
function durationFields(duration: number): { durationMs: number; durationUs: number } {
  const measured = duration > 0 ? duration : 0;
  return { durationMs: Math.round(measured), durationUs: Math.round(measured * 1000) };
}

/**
 * Renders a record's `database` group as the single structured data param of a
 * log call. The JSON sink flattens a lone object param over the entry's context,
 * so a record carries exactly the fields it declares. Mirrors Go's closed
 * `slog.Attr` semantics.
 */
function databaseFields(group: Record<string, unknown>): { database: Record<string, unknown> } {
  return { database: group };
}

/** The migration twin of {@link databaseFields}. */
export function migrationFields(group: Record<string, unknown>): { migration: Record<string, unknown> } {
  return { migration: group };
}

/**
 * Coerce a thrown value into a real `Error` so it reaches the logger's
 * structured `error` field. This is load-bearing, not cosmetic: the JSON sink
 * SKIPS reserved keys (`error` among them) when flattening a data object, so an
 * `{ error: '…' }` field is silently dropped from the JSON output, and a
 * non-Error param would additionally push the record onto the un-flattened
 * `data: [...]` path.
 */
export function asError(value: unknown): Error {
  return value instanceof Error ? value : new Error(String(value));
}

/** The `database` group every query record shares. */
function queryGroup(record: QueryRecord, outcome: string): Record<string, unknown> {
  return {
    operation: record.operation,
    table: record.table,
    datasource: record.datasource ?? DEFAULT_DATASOURCE,
    ...durationFields(record.duration),
    outcome,
    ...(record.rowCount !== undefined && { rowCount: record.rowCount }),
  };
}

/** Emit the contract's DEBUG `query executed` record. */
export function logQueryExecuted(record: QueryRecord): void {
  useLogger(DATABASE_LOGGER).debug(MSG_QUERY_EXECUTED, databaseFields(queryGroup(record, OUTCOME_SUCCESS)));
}

/**
 * Emit the contract's `query failed` record: WARNING, not ERROR. The data layer
 * reports outcome + latency with a structured error and RE-THROWS; the boundary
 * that actually fails owns ERROR exactly once, so a retried-and-recovered query
 * never produces error-level noise. The error travels as a log param — the only
 * way it reaches `entry.error` and therefore the JSON output.
 */
export function logQueryFailed(record: QueryRecord, error: unknown): void {
  useLogger(DATABASE_LOGGER).warn(
    MSG_QUERY_FAILED,
    asError(error),
    databaseFields(queryGroup(record, OUTCOME_FAILURE)),
  );
}

/**
 * Emit the contract's WARNING `slow query` record: the query SUCCEEDED (outcome
 * success) but crossed the configured threshold, which travels as `thresholdMs`
 * instead of being interpolated into the message.
 */
export function logSlowQuery(record: QueryRecord, thresholdMs: number): void {
  useLogger(DATABASE_LOGGER).warn(
    MSG_SLOW_QUERY,
    databaseFields({ ...queryGroup(record, OUTCOME_SUCCESS), thresholdMs }),
  );
}

/**
 * Emit the transaction-boundary record: DEBUG `transaction committed` (outcome
 * success) or WARNING `transaction rolled back` (outcome failure). Unlike
 * {@link logQueryFailed} it attaches NO error: the record carries only the
 * classified, secret-free `rollbackCause`, so a bound parameter value or a row
 * datum embedded in a driver error can never reach a log line.
 */
export function logTransaction(record: TransactionRecord): void {
  const group: Record<string, unknown> = {
    datasource: record.datasource ?? DEFAULT_DATASOURCE,
    outcome: record.outcome === 'rolled-back' ? OUTCOME_FAILURE : OUTCOME_SUCCESS,
    ...durationFields(record.duration),
    retries: record.retries ?? 0,
  };
  if (record.outcome !== 'rolled-back') {
    useLogger(DATABASE_LOGGER).debug(MSG_TX_COMMITTED, databaseFields(group));
    return;
  }
  // The contract requires rollbackCause on every rollback record, so an
  // unclassified cause still reports the `unknown` catch-all rather than leaving
  // the field absent (Go's emitter defaults identically).
  group['rollbackCause'] = record.cause || 'unknown';
  useLogger(DATABASE_LOGGER).warn(MSG_TX_ROLLED_BACK, databaseFields(group));
}

/**
 * Emit the per-migration terminal record on success: INFO with the migration's
 * name, datasource, and elapsed time. One record per migration — the runner's
 * aggregate `migrations applied` summary is a separate, coarser signal.
 */
export function logMigrationApplied(name: string, datasource: string, durationMs: number): void {
  useLogger(DATABASE_MIGRATION_LOGGER).info(
    MSG_MIGRATION_APPLIED,
    migrationFields({ name, datasource, durationMs: Math.round(durationMs), outcome: OUTCOME_SUCCESS }),
  );
}

/**
 * Emit the per-migration terminal record on failure: ERROR with the structured
 * cause. The caller still throws — a migrate run aborting startup is an
 * independent operational signal — so the failure is logged exactly once, here,
 * with the raw cause rather than the wrapper the caller builds.
 */
export function logMigrationFailed(name: string, datasource: string, durationMs: number, error: unknown): void {
  useLogger(DATABASE_MIGRATION_LOGGER).error(
    MSG_MIGRATION_FAILED,
    asError(error),
    migrationFields({ name, datasource, durationMs: Math.round(durationMs), outcome: OUTCOME_FAILURE }),
  );
}
