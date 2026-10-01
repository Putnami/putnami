package database

import (
	"context"
	stderrors "errors"

	"github.com/jackc/pgx/v5/pgconn"

	"go.putnami.dev/errors"
	protocolmigration "go.putnami.dev/protocol/migration"
	"go.putnami.dev/protocol/transaction"
)

// Error codes for the sql package.
const (
	CodeConnection  errors.Code = "db.connection"
	CodeQuery       errors.Code = "db.query"
	CodeTransaction errors.Code = "db.transaction"
	CodeDatasource  errors.Code = "db.datasource"
	CodeBinding     errors.Code = "db.binding"
)

// Migration error codes aligned with the migration protocol taxonomy.
const (
	CodeMigrationApplyFailed    errors.Code = errors.Code(protocolmigration.ErrorCodeApplyFailed)
	CodeMigrationRollbackFailed errors.Code = errors.Code(protocolmigration.ErrorCodeRollbackFailed)
	CodeMigrationLockFailed     errors.Code = errors.Code(protocolmigration.ErrorCodeLockFailed)
	CodeMigrationStateStore     errors.Code = errors.Code(protocolmigration.ErrorCodeStateStoreFailed)
	CodeMigrationStartup        errors.Code = errors.Code(protocolmigration.ErrorCodeStartupBlocked)
	CodeMigrationInvalidDef     errors.Code = errors.Code(protocolmigration.ErrorCodeInvalidDefinition)
)

// PostgreSQL SQLSTATE codes the package classifies into typed transaction
// outcomes. pgx surfaces the SQLSTATE via *pgconn.PgError.Code; these sentinels
// let the CAS / consume-once / rotation helpers turn a driver error into a
// stable transaction.Outcome instead of leaking a raw pg error to callers.
const (
	sqlStateUniqueViolation      = "23505" // unique_violation
	sqlStateSerializationFailure = "40001" // serialization_failure
	sqlStateDeadlockDetected     = "40P01" // deadlock_detected
	sqlStateUndefinedTable       = "42P01" // undefined_table
	sqlStateInvalidSchemaName    = "3F000" // invalid_schema_name
)

// pgErrorCode returns the SQLSTATE carried by err, or "" when err is nil or is
// not (and does not wrap) a *pgconn.PgError. It walks the wrap chain via
// errors.As, so a pg error wrapped by errors.Wrapf is still recognized.
func pgErrorCode(err error) string {
	var pgErr *pgconn.PgError
	if stderrors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// IsSerializationFailure reports whether err is (or wraps) a PostgreSQL
// serialization_failure (40001) — a serialization anomaly under SERIALIZABLE or
// REPEATABLE READ that the caller may safely retry.
func IsSerializationFailure(err error) bool {
	return pgErrorCode(err) == sqlStateSerializationFailure
}

// IsDeadlock reports whether err is (or wraps) a PostgreSQL deadlock_detected
// (40P01) — a deadlock the server broke by aborting this transaction, which the
// caller may safely retry.
func IsDeadlock(err error) bool {
	return pgErrorCode(err) == sqlStateDeadlockDetected
}

// isMissingMigrationStateStore reports whether a read found that the
// migration state-store schema or table has not been created yet. Observational
// migration APIs treat that state as an empty store; mutation APIs still create
// the store under their advisory lock before applying or rolling back.
func isMissingMigrationStateStore(err error) bool {
	switch pgErrorCode(err) {
	case sqlStateUndefinedTable, sqlStateInvalidSchemaName:
		return true
	default:
		return false
	}
}

// txRollbackCause classifies err into a low-cardinality, SECRET-FREE cause code
// for the sql.tx.rollback.<cause> metric and the database.rollbackCause log field.
// It is the load-bearing secret-safety guard for transaction telemetry:
// the returned string is only ever a fixed PostgreSQL SQLSTATE, a context
// sentinel, a curated framework error Code, or "unknown". It NEVER returns
// err.Error(), so a bound parameter value or a row datum embedded in a driver
// error's message can never leak into an attribute or a metric name. Returns ""
// for a nil error (a committed transaction has no cause).
//
// The classification order is precise-to-generic: a SQLSTATE (the exact server
// class) wins over a context sentinel, which wins over the framework Code the
// error was wrapped with, which wins over the "unknown" catch-all.
func txRollbackCause(err error) string {
	if err == nil {
		return ""
	}
	// A PostgreSQL SQLSTATE is a fixed 5-character class code — the most precise
	// and safest cause whenever the driver surfaced one.
	if code := pgErrorCode(err); code != "" {
		return code
	}
	switch {
	case stderrors.Is(err, context.Canceled):
		return "canceled"
	case stderrors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}
	// A framework error carries a stable, curated Code (e.g. "db.transaction").
	// GetCode returns CodeUnknown for a non-framework error, which we treat as
	// the catch-all rather than leak as a real cause.
	if code := errors.GetCode(err); code != "" && code != errors.CodeUnknown {
		return string(code)
	}
	return "unknown"
}

// ClassifyOutcome maps a PostgreSQL SQLSTATE carried by err to the typed
// transaction.Outcome the CAS / consume-once / rotation helpers report, so a
// caught driver error becomes a stable, switchable result code:
//
//   - 40001 (serialization_failure) and 40P01 (deadlock_detected) →
//     OutcomeRetryableSerializationFailure (Outcome.Retryable() is true).
//   - 23505 (unique_violation) → OutcomeAlreadyConsumedConflict.
//
// It returns (outcome, true) when err carries one of those recognized
// SQLSTATEs, and ("", false) otherwise — including a nil err. A false ok means
// "not a classified business outcome": the caller must treat err as a genuine
// I/O failure and surface it raw rather than as a typed Outcome.
func ClassifyOutcome(err error) (transaction.Outcome, bool) {
	switch pgErrorCode(err) {
	case sqlStateSerializationFailure, sqlStateDeadlockDetected:
		return transaction.OutcomeRetryableSerializationFailure, true
	case sqlStateUniqueViolation:
		return transaction.OutcomeAlreadyConsumedConflict, true
	default:
		return "", false
	}
}
