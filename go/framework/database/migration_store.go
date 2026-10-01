package database

import (
	"context"
	"crypto/sha256"
	stdsql "database/sql"
	"encoding/hex"
	"sort"
	"time"

	perrors "go.putnami.dev/errors"
	"go.putnami.dev/migration"
	protocolmigration "go.putnami.dev/protocol/migration"
)

// This file owns the canonical migration.migrations state store: the row
// type, the embedded SQL, the read/write helpers, and the small utilities
// that hash, order, and adapt rows. migration.go keeps the Migrator
// engine and the authoring types. No behavior is defined here that the
// engine does not drive — the split is purely organizational.

// Migration is one row of the canonical migration.migrations state store.
type Migration struct {
	ID              string
	DBName          string
	Name            string
	Hash            string
	ExecutedAt      string
	ExecutionTimeMs int64
	Success         int
	ErrorMessage    string
	DownSQL         string
	DownHash        string
}

func ensureStateStore(ctx context.Context, conn *stdsql.Conn) error {
	if _, err := conn.ExecContext(ctx, `CREATE SCHEMA IF NOT EXISTS migration`); err != nil {
		return perrors.Wrapf(err, CodeMigrationStateStore, "create migration schema")
	}
	if _, err := conn.ExecContext(ctx, createStateTableSQL); err != nil {
		return perrors.Wrapf(err, CodeMigrationStateStore, "create migration state table")
	}
	return nil
}

func listMigrations(ctx context.Context, conn *stdsql.Conn, dbName string, successOnly bool) ([]Migration, error) {
	query := listAllSQL
	if successOnly {
		query = listSuccessSQL
	}
	rows, err := conn.QueryContext(ctx, query, dbName)
	if err != nil {
		return nil, perrors.Wrapf(err, CodeMigrationStateStore, "list migrations")
	}
	defer rows.Close() //nolint:errcheck // best-effort close

	var out []Migration
	for rows.Next() {
		var mig Migration
		var errMsg, downSQL, downHash stdsql.NullString
		if err := rows.Scan(
			&mig.ID, &mig.DBName, &mig.Name, &mig.Hash, &mig.ExecutedAt,
			&mig.ExecutionTimeMs, &mig.Success, &errMsg, &downSQL, &downHash,
		); err != nil {
			return nil, perrors.Wrapf(err, CodeMigrationStateStore, "scan migration row")
		}
		mig.ErrorMessage = errMsg.String
		mig.DownSQL = downSQL.String
		mig.DownHash = downHash.String
		out = append(out, mig)
	}
	if err := rows.Err(); err != nil {
		return nil, perrors.Wrapf(err, CodeMigrationStateStore, "iterate migration rows")
	}
	return out, nil
}

func sortByExecutedAtDesc(records []Migration) {
	sort.SliceStable(records, func(i, j int) bool {
		if records[i].ExecutedAt != records[j].ExecutedAt {
			return records[i].ExecutedAt > records[j].ExecutedAt
		}
		return records[i].Name > records[j].Name
	})
}

func withTx(ctx context.Context, conn *stdsql.Conn, fn func(*stdsql.Tx) error) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback() //nolint:errcheck // surface the inner error
		return err
	}
	return tx.Commit()
}

// applySearchPath sets the search_path for the current transaction only
// (is_local = true) so a migration body's unqualified DDL resolves against the
// datasource's declared schema, then reverts at COMMIT/ROLLBACK — never leaking
// onto the pooled connection. A blank schema is a no-op, leaving the
// connection's own search_path (from the pool/binding) in force; that is the
// path the published bundle and the test provider deliberately rely on. The
// state-store SQL is fully qualified (migration.migrations), so it is unaffected
// either way. It must run as the first statement in the body's transaction.
func applySearchPath(ctx context.Context, tx *stdsql.Tx, schema string) error {
	if schema == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx, setSearchPathSQL, schema)
	return err
}

// clearStatementTimeout disables the per-statement timeout for the current
// transaction only (is_local = true). The migration runner opens its
// connection from the same pgxpool as the app, so migration statements would
// otherwise inherit the pool's statement_timeout (30s default) and a single
// long DDL statement (large CREATE INDEX, partition swap) would be aborted with
// SQLSTATE 57014 — contradicting the documented guarantee that long migrations
// run to completion. Being transaction-local, it reverts at COMMIT/ROLLBACK and
// never leaks onto the pooled connection. It must run inside the body's
// transaction, before the migration statement.
func clearStatementTimeout(ctx context.Context, tx *stdsql.Tx) error {
	_, err := tx.ExecContext(ctx, clearStatementTimeoutSQL)
	return err
}

func sha256Hex(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// advisoryLockKey returns the literal passed to hashtext() so the
// resulting pg_advisory_lock key is computed server-side. Both Go and
// TypeScript runners pass the same literal, so they take the same lock
// for one datasource.
func advisoryLockKey(dbName string) string {
	return "putnami.migration:" + dbName
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func resolveDatasource(primary, fallback string) string {
	if primary != "" {
		return primary
	}
	if fallback != "" {
		return fallback
	}
	return protocolmigration.DefaultDatasource
}

// recordFromMigration adapts a state-store row into the cross-kind
// migration.Record shape, used by Verify and the CLI's status output.
// A malformed ExecutedAt yields a zero time in the Record — the
// state-store writer always emits RFC3339, so this can only happen for
// rows written by a future protocol revision that changes the column
// shape; we surface zero rather than fail the read.
func recordFromMigration(m Migration, namespace, target, source string) migration.Record {
	var executedAt time.Time
	if m.ExecutedAt != "" {
		if t, err := time.Parse(time.RFC3339, m.ExecutedAt); err == nil {
			executedAt = t
		}
	}
	status := migration.StatusApplied
	if m.Success == 0 {
		status = migration.StatusFailed
	}
	return migration.Record{
		Kind:       migration.KindSQL,
		Namespace:  namespace,
		Name:       m.Name,
		Status:     status,
		Hash:       m.Hash,
		DownHash:   m.DownHash,
		ExecutedAt: executedAt,
		DurationMs: m.ExecutionTimeMs,
		Error:      m.ErrorMessage,
		Source:     source,
		Target:     target,
	}
}

const (
	acquireAdvisoryLockSQL = `SELECT pg_advisory_lock(hashtext($1)::bigint)`
	releaseAdvisoryLockSQL = `SELECT pg_advisory_unlock(hashtext($1)::bigint)`
)

// setSearchPathSQL sets search_path for the current transaction only. set_config
// is used over a plain SET so the schema travels as a bound parameter ($1)
// rather than an interpolated identifier; the third argument (is_local = true)
// scopes the change to the transaction.
const setSearchPathSQL = `SELECT set_config('search_path', $1, true)`

// clearStatementTimeoutSQL disables statement_timeout for the current
// transaction only (is_local = true). The '0' value is a constant, so no
// interpolation is involved.
const clearStatementTimeoutSQL = `SELECT set_config('statement_timeout', '0', true)`

const createStateTableSQL = `
CREATE TABLE IF NOT EXISTS migration.migrations (
    id TEXT PRIMARY KEY,
    db_name TEXT NOT NULL,
    name TEXT NOT NULL,
    hash TEXT NOT NULL,
    executed_at TEXT NOT NULL,
    execution_time_ms INTEGER NOT NULL,
    success INTEGER NOT NULL,
    error_message TEXT,
    down_sql TEXT,
    down_hash TEXT
)`

const listSuccessSQL = `
SELECT id, db_name, name, hash, executed_at, execution_time_ms, success,
       error_message, down_sql, down_hash
FROM migration.migrations
WHERE db_name = $1 AND success = 1
ORDER BY executed_at ASC, name ASC`

const listAllSQL = `
SELECT id, db_name, name, hash, executed_at, execution_time_ms, success,
       error_message, down_sql, down_hash
FROM migration.migrations
WHERE db_name = $1
ORDER BY executed_at ASC, name ASC`

const upsertSuccessSQL = `
INSERT INTO migration.migrations (
    id, db_name, name, hash, executed_at, execution_time_ms, success,
    error_message, down_sql, down_hash
) VALUES ($1, $2, $3, $4, $5, $6, 1, NULL, $7, $8)
ON CONFLICT (id) DO UPDATE SET
    db_name = EXCLUDED.db_name,
    name = EXCLUDED.name,
    hash = EXCLUDED.hash,
    executed_at = EXCLUDED.executed_at,
    execution_time_ms = EXCLUDED.execution_time_ms,
    success = 1,
    error_message = NULL,
    down_sql = EXCLUDED.down_sql,
    down_hash = EXCLUDED.down_hash`

const upsertFailureSQL = `
INSERT INTO migration.migrations (
    id, db_name, name, hash, executed_at, execution_time_ms, success,
    error_message, down_sql, down_hash
) VALUES ($1, $2, $3, $4, $5, $6, 0, $7, $8, $9)
ON CONFLICT (id) DO UPDATE SET
    db_name = EXCLUDED.db_name,
    name = EXCLUDED.name,
    hash = EXCLUDED.hash,
    executed_at = EXCLUDED.executed_at,
    execution_time_ms = EXCLUDED.execution_time_ms,
    success = 0,
    error_message = EXCLUDED.error_message,
    down_sql = EXCLUDED.down_sql,
    down_hash = EXCLUDED.down_hash`
