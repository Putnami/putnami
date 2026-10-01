package database

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/logger/logtest"
	pdb "go.putnami.dev/protocol/database"
	diag "go.putnami.dev/protocol/diagnostic"
)

// This file executes the database AND migration boundaries' cases of the
// canonical cross-runtime log corpus (protocols/logging/conformance) against the
// records the REAL pool / Migrator emit through the REAL JSON sink. Its
// TypeScript twin is
// typescript/framework/database/test/logging-cross-language.test.ts.
//
// The migration cases live here (not in go.putnami.dev/migration) because this is
// the module that emits them: go/framework/migration owns only the registry's
// records, and it cannot import this module — the dependency runs the other way.
const logConformanceManifest = "../../../protocols/logging/conformance/manifest.json"

// envTestBinding is the canonical test-database binding variable, duplicated here
// rather than imported: go.putnami.dev/database/testprovider imports THIS package,
// so an internal test cannot import it back. The gate is the same as
// conformance_test.go's corpus runner — no binding, no database cases.
const envTestBinding = "DATABASE_TEST_BINDINGS"

// logConformancePool connects a real Pool to the first datasource of the injected
// test binding, with its logger pointed at the recorder, or SKIPS when no binding
// is present (so the local unit gate stays green with no Postgres).
//
// It deliberately does not provision an isolated database: every statement the
// database cases run is a read (SELECT 1, pg_sleep, one deliberately invalid
// statement) or a transaction over reads, so the records under test need no
// schema of their own.
func logConformancePool(t *testing.T, rec *logtest.Recorder, slowQuery time.Duration) *Pool {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv(envTestBinding))
	if raw == "" {
		t.Skipf("%s not set; skipping the database cases of the logging conformance corpus", envTestBinding)
	}
	tb, diags := pdb.ParseAndValidateTestBinding([]byte(raw))
	if diag.HasErrors(diags) {
		t.Fatalf("invalid %s: %v", envTestBinding, diags)
	}
	if tb == nil || len(tb.Databases) == 0 {
		t.Skipf("%s carries no datasource; skipping the database cases", envTestBinding)
	}
	names := make([]string, 0, len(tb.Databases))
	for name := range tb.Databases {
		names = append(names, name)
	}
	sort.Strings(names)
	name := names[0]
	entry := tb.Databases[name]

	binding := &pdb.Binding{
		ProtocolVersion: pdb.ProtocolVersion,
		Databases: map[string]pdb.Database{
			name: {Engine: pdb.EnginePostgres, Schema: entry.Schema, Connection: entry.Connection},
		},
	}
	cfg, err := PoolConfigFromBinding(binding, name)
	if err != nil {
		t.Fatalf("PoolConfigFromBinding(%q): %v", name, err)
	}
	cfg.SlowQueryThreshold = slowQuery

	pool, err := NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect datasource %q: %v", name, err)
	}
	t.Cleanup(pool.Close)
	pool.log = rec.Named(databaseLoggerName)
	return pool
}

// The database cases share one drive: a real pool, one statement per case. They
// run in one test so a single connection serves them all; the recorder is reset
// between cases so each case's record is unambiguous.
func TestLoggingConformanceDatabaseRecords(t *testing.T) {
	suite := logtest.LoadCases(t, logConformanceManifest, "database")

	rec := logtest.NewRecorder(t)
	// Two pools, because Go's slow-query escalation REPLACES the "query executed"
	// record rather than adding to it: with a threshold configured, a query that
	// happens to cross it emits "slow query" instead — so the non-slow cases run on
	// a pool with the escalation disabled and only the slow case configures the 1ms
	// threshold the corpus pins.
	pool := logConformancePool(t, rec, 0)
	slowPool := logConformancePool(t, rec, time.Millisecond)
	ctx := context.Background()

	// db.query.success — the smallest possible read. Background context: the
	// corpus requires NO traceId on database records.
	want := suite.Case(t, "db.query.success")
	rec.Reset()
	var one int
	if err := pool.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
		t.Fatalf("SELECT 1: %v", err)
	}
	logtest.AssertRecord(t, rec.Record(t, want), want)

	// db.query.failure — a syntactically valid but unresolvable relation. The pool
	// reports WARNING and PROPAGATES; the boundary owns ERROR.
	want = suite.Case(t, "db.query.failure")
	rec.Reset()
	if _, err := pool.Exec(ctx, "SELECT * FROM putnami_logging_conformance_missing"); err == nil {
		t.Fatal("expected the invalid statement to fail")
	}
	logtest.AssertRecord(t, rec.Record(t, want), want)

	// db.query.slow — the query SUCCEEDS but crosses the 1ms threshold. pg_sleep(10ms)
	// makes that deterministic rather than latency-dependent.
	want = suite.Case(t, "db.query.slow")
	rec.Reset()
	if _, err := slowPool.Exec(ctx, "SELECT pg_sleep(0.01)"); err != nil {
		t.Fatalf("pg_sleep: %v", err)
	}
	logtest.AssertRecord(t, rec.Record(t, want), want)

	// db.tx.commit — a real transaction boundary with one write, committed. The
	// write targets a TEMP table so the case needs no schema of its own: it lives
	// in the session and vanishes with it.
	want = suite.Case(t, "db.tx.commit")
	rec.Reset()
	if err := WithTx(ctx, pool, func(txCtx context.Context) error {
		return txWrite(txCtx, pool)
	}); err != nil {
		t.Fatalf("WithTx commit: %v", err)
	}
	logtest.AssertRecord(t, rec.Record(t, want), want)

	// db.tx.rollback — the callback errors, so the boundary rolls back and reports
	// the CLASSIFIED cause only (never the error text).
	want = suite.Case(t, "db.tx.rollback")
	rec.Reset()
	rollbackErr := fmt.Errorf("conformance rollback")
	if err := WithTx(ctx, pool, func(txCtx context.Context) error {
		if err := txWrite(txCtx, pool); err != nil {
			return err
		}
		return rollbackErr
	}); err == nil {
		t.Fatal("expected WithTx to propagate the callback error")
	}
	rollback := rec.Record(t, want)
	logtest.AssertRecord(t, rollback, want)
	if strings.Contains(rollback, rollbackErr.Error()) {
		t.Errorf("the rollback record must carry the classified cause only, got %s", rollback)
	}
}

// txWrite performs the transaction cases' write. A TEMP table keeps the case
// self-contained (session-scoped, dropped with the session), so the database cases
// need no schema and leave nothing behind in the target database.
func txWrite(ctx context.Context, pool *Pool) error {
	if _, err := pool.Exec(ctx, "CREATE TEMP TABLE IF NOT EXISTS putnami_logging_conformance_tx (id int)"); err != nil {
		return err
	}
	_, err := pool.Exec(ctx, "INSERT INTO putnami_logging_conformance_tx (id) VALUES (1)")
	return err
}

// The migration cases are driven through the real Migrator over the package's
// DB-free recording connection (the same seam migration_success_test.go uses), so
// they run in the ordinary unit gate: a migration record is a contract too, and
// waiting for a Postgres-enabled run to catch drift in it would be the slow way
// to find out. The TypeScript twin drives its real Migrator over a stub
// datasource for the same reason.
func TestLoggingConformanceMigrationApplied(t *testing.T) {
	want := logtest.LoadCases(t, logConformanceManifest, "migration").Case(t, "migration.applied")

	db, _ := newFakeMigratorDB(t)
	rec := logtest.NewRecorder(t)
	m := NewMigrator(db, MigrationConfig{
		Datasource:  "primary",
		Definitions: []Definition{{Name: "0001_conformance", SQL: "CREATE TABLE conformance (id int)"}},
	})
	m.log = rec.Named(migrationLoggerName)

	if _, err := m.Up(context.Background()); err != nil {
		t.Fatalf("Up: %v", err)
	}

	logtest.AssertRecord(t, rec.Record(t, want), want)
}

func TestLoggingConformanceMigrationFailed(t *testing.T) {
	want := logtest.LoadCases(t, logConformanceManifest, "migration").Case(t, "migration.failed")

	db, store := newFakeMigratorDB(t)
	const body = "CREATE TABLE broken ("
	store.failBody(body, fmt.Errorf("syntax error at end of input"))

	rec := logtest.NewRecorder(t)
	m := NewMigrator(db, MigrationConfig{
		Datasource:  "primary",
		Definitions: []Definition{{Name: "0002_conformance_failing", SQL: body}},
	})
	m.log = rec.Named(migrationLoggerName)

	if _, err := m.Up(context.Background()); err == nil {
		t.Fatal("Up must still return the apply error after logging it")
	}

	logtest.AssertRecord(t, rec.Record(t, want), want)
}
