/**
 * Custom error types for SQL module
 */

import { Outcome } from './transaction-outcome';

/**
 * PostgreSQL SQLSTATE codes the package classifies. These sentinels let both
 * {@link RepositoryError.fromDatabaseError} and {@link classifyOutcome} turn a
 * driver error into a stable, typed value instead of leaking a raw pg error.
 * They mirror the Go adapter's `sqlState*` constants in
 * `go/framework/database/errors.go`.
 */
export const SqlState = {
  /** unique_violation */
  UniqueViolation: '23505',
  /** foreign_key_violation */
  ForeignKeyViolation: '23503',
  /** not_null_violation */
  NotNullViolation: '23502',
  /** check_violation */
  CheckViolation: '23514',
  /** serialization_failure — a serialization anomaly the caller may retry. */
  SerializationFailure: '40001',
  /** deadlock_detected — the server broke a deadlock; the caller may retry. */
  DeadlockDetected: '40P01',
} as const;

/**
 * Base error class for repository operations
 */
export const RepositoryErrorCode = {
  EmptyEntity: 'EMPTY_ENTITY',
  MissingPrimaryKey: 'MISSING_PRIMARY_KEY',
  ValidationError: 'VALIDATION_ERROR',
  SaveFailed: 'SAVE_FAILED',
  SaveError: 'SAVE_ERROR',
  EmptyEntities: 'EMPTY_ENTITIES',
  DeleteWithoutFilters: 'DELETE_WITHOUT_FILTERS',
  UniqueViolation: 'UNIQUE_VIOLATION',
  ForeignKeyViolation: 'FOREIGN_KEY_VIOLATION',
  NotNullViolation: 'NOT_NULL_VIOLATION',
  CheckViolation: 'CHECK_VIOLATION',
  SerializationFailure: 'SERIALIZATION_FAILURE',
  DeadlockDetected: 'DEADLOCK_DETECTED',
  NotFound: 'NOT_FOUND',
  Unknown: 'UNKNOWN',
} as const;

export type RepositoryErrorCode = (typeof RepositoryErrorCode)[keyof typeof RepositoryErrorCode];

/** Map PostgreSQL SQLSTATE codes to typed repository error codes. */
const PG_CODE_TO_REPOSITORY_CODE: Record<string, RepositoryErrorCode> = {
  [SqlState.UniqueViolation]: RepositoryErrorCode.UniqueViolation,
  [SqlState.ForeignKeyViolation]: RepositoryErrorCode.ForeignKeyViolation,
  [SqlState.NotNullViolation]: RepositoryErrorCode.NotNullViolation,
  [SqlState.CheckViolation]: RepositoryErrorCode.CheckViolation,
  [SqlState.SerializationFailure]: RepositoryErrorCode.SerializationFailure,
  [SqlState.DeadlockDetected]: RepositoryErrorCode.DeadlockDetected,
};

/**
 * Extract the PostgreSQL SQLSTATE carried by a thrown value, or `undefined` when
 * it is not a postgres error. postgres.js surfaces the SQLSTATE as the error's
 * `code` string. This reads the driver error the CAS helpers catch directly from
 * `sql.unsafe(...)` — it is not fed an already-wrapped {@link RepositoryError}
 * (whose `code` is a {@link RepositoryErrorCode}, not a SQLSTATE). It mirrors the
 * Go adapter's `pgErrorCode` for the raw driver error.
 */
export function pgErrorCode(err: unknown): string | undefined {
  if (err && typeof err === 'object') {
    const code = (err as Record<string, unknown>)['code'];
    if (typeof code === 'string') return code;
  }
  return undefined;
}

/**
 * Map a PostgreSQL SQLSTATE carried by `err` to the typed transaction
 * {@link Outcome} the CAS / consume-once / rotation helpers report, so a caught
 * driver error becomes a stable, switchable result code. Mirrors the Go adapter's
 * `ClassifyOutcome` (`go/framework/database/errors.go`) byte-for-byte:
 *
 *   - `40001` (serialization_failure) and `40P01` (deadlock_detected) →
 *     {@link Outcome.RetryableSerializationFailure} ({@link outcomeRetryable} is true).
 *   - `23505` (unique_violation) → {@link Outcome.AlreadyConsumedConflict}.
 *
 * Returns the outcome for one of those recognized SQLSTATEs, and `undefined`
 * otherwise (including a nil/undefined error). An `undefined` result means "not a
 * classified business outcome": the caller must treat `err` as a genuine I/O
 * failure and surface it raw rather than as a typed Outcome.
 */
export function classifyOutcome(err: unknown): Outcome | undefined {
  switch (pgErrorCode(err)) {
    case SqlState.SerializationFailure:
    case SqlState.DeadlockDetected:
      return Outcome.RetryableSerializationFailure;
    case SqlState.UniqueViolation:
      return Outcome.AlreadyConsumedConflict;
    default:
      return undefined;
  }
}

/**
 * Classify a thrown value into a low-cardinality, SECRET-FREE rollback cause
 * code for the `sql.tx.rollback.<cause>` metric and the `rollbackCause` log
 * attribute. It is the load-bearing secret-safety guard for transaction
 * telemetry: the returned string is only ever a fixed PostgreSQL
 * SQLSTATE, a {@link RepositoryErrorCode}, an error class name, or `'unknown'`.
 * It NEVER returns the error's `message`, so a bound parameter value or a row
 * datum embedded in a driver error's message can never leak into a metric name
 * or a log attribute. Returns `''` for a nullish value (a committed transaction
 * has no cause). Mirrors the Go adapter's `txRollbackCause`
 * (`go/framework/database/errors.go`) classification order: precise-to-generic.
 */
export function classifyRollbackCause(error: unknown): string {
  if (error === undefined || error === null) return '';
  // A wrapped RepositoryError carries the driver SQLSTATE (most precise) or its
  // typed RepositoryErrorCode — both are curated, secret-free labels.
  if (error instanceof RepositoryError) return error.pgCode ?? error.code;
  // A raw driver error surfaces its SQLSTATE as `code`.
  const code = pgErrorCode(error);
  if (code) return code;
  // Fall back to the error class name (e.g. "Error", "AggregateError") — a
  // type-level label, never the message.
  if (error instanceof Error) return error.name || 'error';
  return 'unknown';
}

function asErrorCause(cause: unknown): Error {
  if (cause instanceof Error) return cause;
  if (cause && typeof cause === 'object') {
    const record = cause as Record<string, unknown>;
    const message = typeof record['message'] === 'string' ? record['message'] : String(cause);
    return Object.assign(new Error(message), record);
  }
  return new Error(String(cause));
}

export class RepositoryError extends Error {
  /** PostgreSQL error code (e.g. "23505" for unique_violation), if the cause is a postgres error. */
  public pgCode?: string;
  /** Violated constraint name, when the postgres error identifies one. */
  public constraint?: string;
  /** Offending column, when the postgres error identifies one. */
  public column?: string;

  constructor(
    message: string,
    public code: RepositoryErrorCode = RepositoryErrorCode.Unknown,
    public override cause?: Error,
  ) {
    super(message);
    this.name = 'RepositoryError';
    // Preserve postgres error metadata for structured error handling.
    if (cause && typeof cause === 'object') {
      const pg = cause as unknown as Record<string, unknown>;
      if (typeof pg['code'] === 'string') this.pgCode = pg['code'];
      if (typeof pg['constraint'] === 'string') this.constraint = pg['constraint'];
      if (typeof pg['constraint_name'] === 'string') this.constraint = pg['constraint_name'];
      if (typeof pg['column'] === 'string') this.column = pg['column'];
      if (typeof pg['column_name'] === 'string') this.column = pg['column_name'];
    }
    // Maintains proper stack trace for where our error was thrown (only available on V8)
    if (Error.captureStackTrace) {
      Error.captureStackTrace(this, RepositoryError);
    }
  }

  /**
   * Build a RepositoryError from a thrown database error, mapping the
   * PostgreSQL SQLSTATE code to a typed {@link RepositoryErrorCode} (unique,
   * foreign-key, not-null, or check violation). Unrecognised codes fall back
   * to `SAVE_ERROR`. The `constraint`/`column` fields are parsed from the
   * postgres error when present.
   */
  static fromDatabaseError(message: string, cause: unknown): RepositoryError {
    const err = asErrorCause(cause);
    const pgCode = (err as unknown as Record<string, unknown>)['code'];
    const code =
      (typeof pgCode === 'string' ? PG_CODE_TO_REPOSITORY_CODE[pgCode] : undefined) ?? RepositoryErrorCode.SaveError;
    return new RepositoryError(message, code, err);
  }
}

/**
 * Error class for migration operations
 */
export class MigrationError extends Error {
  constructor(
    message: string,
    public migration: string,
    public override cause?: Error,
  ) {
    super(message);
    this.name = 'MigrationError';
    // Maintains proper stack trace for where our error was thrown (only available on V8)
    if (Error.captureStackTrace) {
      Error.captureStackTrace(this, MigrationError);
    }
  }
}
