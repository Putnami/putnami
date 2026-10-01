package database

import (
	"context"
	"log/slog"
	"time"

	"go.putnami.dev/logger"
)

// ---------------------------------------------------------------------------
// Database boundary log contract — the Go twin of
// typescript/framework/database/src/observability/database-logging.ts.
//
// Every record shape of the database boundary (pool queries, transactions, and
// migrations) is built here exactly once, so no call site can invent a message,
// a severity, or a field name. The contract itself is normative in
// protocols/logging/conformance (manifest + README); both runtimes must emit
// schema-equivalent records for the same boundary event.
// ---------------------------------------------------------------------------

// Pinned logger names of the contract ("Pinned logger names": dots, never
// colons). databaseLoggerName owns the pool's query, transaction, and
// utilization records; migrationLoggerName owns every migration record (the
// Migrator, the SQL runner, and — in go.putnami.dev/migration — the registry).
const (
	databaseLoggerName  = "database"
	migrationLoggerName = "database.migration"
)

// Outcome vocabulary of the logging contract. Database records only ever report
// success or failure; a rolled-back transaction reports failure and carries the
// CLASSIFIED reason in rollbackCause (never raw error text).
const (
	outcomeSuccess = "success"
	outcomeFailure = "failure"
)

// Stable record messages. They are constants because log-based metrics and
// alerts match on them: no identifier, duration, threshold, or error text is
// ever interpolated into a message.
const (
	msgQueryExecuted    = "query executed"
	msgQueryFailed      = "query failed"
	msgSlowQuery        = "slow query"
	msgTxCommitted      = "transaction committed"
	msgTxRolledBack     = "transaction rolled back"
	msgMigrationApplied = "migration applied"
	msgMigrationFailed  = "migration failed"
	msgPoolUtilization  = "pool utilization"
)

// databaseLoggerFrom derives the pinned "database" logger from root. The pool
// resolves it once at construction, never per query.
func databaseLoggerFrom(root *logger.Logger) *logger.Logger {
	return root.Named(databaseLoggerName)
}

// migrationLoggerFrom derives the pinned "database.migration" logger from root.
// The Migrator and the SQL runner resolve it once at construction; tests use it
// with a memory-sink root logger to capture the records one run emits.
func migrationLoggerFrom(root *logger.Logger) *logger.Logger {
	return root.Named(migrationLoggerName)
}

// databaseAttr renders a record's "database" group as one closed attr. An attr
// replaces a same-named context key at the sink, so each record carries exactly
// the fields it declares — never leftovers of another record's group.
func databaseAttr(group map[string]any) slog.Attr {
	return slog.Any("database", group)
}

// migrationAttr renders a record's "migration" group as one closed attr, the
// migration twin of databaseAttr.
func migrationAttr(group map[string]any) slog.Attr {
	return slog.Any("migration", group)
}

// queryLogOperation maps the exported QueryOp label to the camelCase operation
// value of the log contract. The QueryOp constants stay the observer/metric
// labels they have always been ("query_row"), exactly as the exported
// TxOutcome stays committed/rolled-back while the record reports
// success/failure: the log vocabulary is derived here, never at a call site.
func queryLogOperation(op QueryOp) string {
	if op == QueryOpQueryRow {
		return "queryRow"
	}
	return string(op)
}

// queryGroup builds the "database" group every query record shares: the
// operation, the datasource (defaulted to the canonical default datasource so
// the field is always present, matching the TypeScript adapter), both duration
// units the contract requires for database records, and the outcome.
func queryGroup(op QueryOp, datasource string, dur time.Duration, outcome string) map[string]any {
	return map[string]any{
		"operation":  queryLogOperation(op),
		"datasource": resolveDatasource(datasource, ""),
		"durationMs": dur.Milliseconds(),
		"durationUs": dur.Microseconds(),
		"outcome":    outcome,
	}
}

// logQueryExecuted emits the DEBUG "query executed" record of a normal query.
// The pgx.ErrNoRows not-found sentinel routes here too: it is the expected
// outcome of a "look it up, 404 if absent" lookup, not a failure.
func logQueryExecuted(ctx context.Context, log *logger.Logger, op QueryOp, datasource string, dur time.Duration) {
	log.DebugCtx(ctx, msgQueryExecuted, databaseAttr(queryGroup(op, datasource, dur, outcomeSuccess)))
}

// logQueryFailed emits the WARNING "query failed" record. WARNING, not ERROR,
// is the contract's severity policy: the pool reports outcome + latency with a
// structured error and PROPAGATES the error, while the boundary that actually
// fails owns ERROR exactly once — so a retried-and-recovered query never
// produces error-level noise. The error travels as the logger's structured
// error attr (name/message/…), never as a stringified attribute.
func logQueryFailed(ctx context.Context, log *logger.Logger, op QueryOp, datasource string, dur time.Duration, err error) {
	log.WarnCtx(ctx, msgQueryFailed,
		databaseAttr(queryGroup(op, datasource, dur, outcomeFailure)),
		logger.ErrorAttr(err))
}

// logSlowQuery emits the WARNING "slow query" record: the query SUCCEEDED
// (outcome success) but crossed the configured threshold, which travels as
// thresholdMs rather than being interpolated into the message.
func logSlowQuery(ctx context.Context, log *logger.Logger, op QueryOp, datasource string, dur, threshold time.Duration) {
	group := queryGroup(op, datasource, dur, outcomeSuccess)
	group["thresholdMs"] = threshold.Milliseconds()
	log.WarnCtx(ctx, msgSlowQuery, databaseAttr(group))
}

// logTransaction emits the transaction-boundary record: DEBUG "transaction
// committed" (outcome success) or WARNING "transaction rolled back" (outcome
// failure). Unlike logQueryFailed it attaches NO error: a rollback carries only
// the classified, secret-free RollbackCause, so a bound parameter value or a row
// datum embedded in a driver error can never reach a log line.
func logTransaction(ctx context.Context, log *logger.Logger, obs TxObservation) {
	group := map[string]any{
		"datasource": resolveDatasource(obs.Datasource, ""),
		"outcome":    outcomeSuccess,
		"durationMs": obs.Duration.Milliseconds(),
		"durationUs": obs.Duration.Microseconds(),
		"retries":    obs.Retries,
	}
	if obs.Outcome != TxOutcomeRolledBack {
		log.DebugCtx(ctx, msgTxCommitted, databaseAttr(group))
		return
	}
	group["outcome"] = outcomeFailure
	// The contract requires rollbackCause on every rollback record. Every call
	// site classifies one today (txRollbackCause falls back to "unknown", and the
	// panic / rollback-only / explicit-rollback paths pass sentinels); defaulting
	// here keeps the field present — and identical to the TypeScript adapter's
	// `metrics.cause || 'unknown'` — without depending on that.
	cause := obs.RollbackCause
	if cause == "" {
		cause = "unknown"
	}
	group["rollbackCause"] = cause
	log.WarnCtx(ctx, msgTxRolledBack, databaseAttr(group))
}

// logMigrationApplied emits the per-migration terminal record on success: INFO
// with the migration's name, datasource, elapsed time, and outcome. One record
// per migration — the runner's aggregate "migrations applied" summary is a
// separate, coarser signal.
func logMigrationApplied(ctx context.Context, log *logger.Logger, name, datasource string, elapsed time.Duration) {
	log.InfoCtx(ctx, msgMigrationApplied, migrationAttr(map[string]any{
		"name":       name,
		"datasource": datasource,
		"durationMs": elapsed.Milliseconds(),
		"outcome":    outcomeSuccess,
	}))
}

// logMigrationFailed emits the per-migration terminal record on failure: ERROR
// with the structured cause. The caller still returns the wrapped error — a
// migrate run aborting startup is an independent operational signal — so the
// failure is logged exactly once, here, with the raw cause rather than the
// wrapper the caller builds.
func logMigrationFailed(ctx context.Context, log *logger.Logger, name, datasource string, elapsed time.Duration, cause error) {
	log.ErrorCtx(ctx, msgMigrationFailed, cause, migrationAttr(map[string]any{
		"name":       name,
		"datasource": datasource,
		"durationMs": elapsed.Milliseconds(),
		"outcome":    outcomeFailure,
	}))
}
