package database

import (
	"go.putnami.dev/protocol/features/spectest"

	"context"
	stdsql "database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// --- Recording fake sql/driver for migration success paths -----------------
//
// The existing test_migrator driver only returns errors, so every Migrator
// success path (apply, idempotency skip, lexicographic ordering, Up/RollbackTo,
// and the rollback down-hash integrity check) was unexercised. This fake backs
// a *sql.DB with an in-memory stand-in for the migration.migrations table: it
// records the up/down bodies it executes and serves rows back to the engine.
// It is wired via sql.OpenDB(connector) so each test gets an isolated store
// without touching the process-global driver registry.

// fakeStore is the in-memory migration.migrations table shared across the
// connections a single *sql.DB hands out.
type fakeStore struct {
	mu            sync.Mutex
	rows          []Migration      // applied/failed state-store rows
	execLog       []string         // recorded migration up/down bodies, in execution order
	statementLog  []string         // every Exec statement, including state-store DDL and locks
	searchPaths   []string         // schema arg of each set_config('search_path', …) call, in order
	timeoutClears int              // number of set_config('statement_timeout','0',…) calls
	failBodies    map[string]error // migration bodies that must fail (nil = every body succeeds)
	queryErr      error            // optional error returned by state-store reads
}

// failBody makes the named migration body fail with err, so the failure path of
// an apply (bookkeeping row + terminal record) can be driven without a live
// database. A body with no entry keeps succeeding.
func (s *fakeStore) failBody(body string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failBodies == nil {
		s.failBodies = make(map[string]error)
	}
	s.failBodies[body] = err
}

func (s *fakeStore) exec(query string, args []driver.NamedValue) (driver.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statementLog = append(s.statementLog, query)
	switch query {
	case `CREATE SCHEMA IF NOT EXISTS migration`, createStateTableSQL,
		acquireAdvisoryLockSQL, releaseAdvisoryLockSQL:
		return fakeResult{}, nil
	case setSearchPathSQL:
		// Capture the schema set as the transaction-local search_path,
		// separately from the migration bodies so tests can assert both.
		s.searchPaths = append(s.searchPaths, argStr(args, 0))
		return fakeResult{}, nil
	case clearStatementTimeoutSQL:
		// Count the transaction-local statement_timeout disable, kept out of the
		// recorded bodies so tests can assert it ran without perturbing bodyLog.
		s.timeoutClears++
		return fakeResult{}, nil
	case upsertSuccessSQL:
		s.putLocked(Migration{
			ID: argStr(args, 0), DBName: argStr(args, 1), Name: argStr(args, 2),
			Hash: argStr(args, 3), ExecutedAt: argStr(args, 4), ExecutionTimeMs: argInt64(args, 5),
			Success: 1, DownSQL: argStr(args, 6), DownHash: argStr(args, 7),
		})
		return fakeResult{rows: 1}, nil
	case upsertFailureSQL:
		s.putLocked(Migration{
			ID: argStr(args, 0), DBName: argStr(args, 1), Name: argStr(args, 2),
			Hash: argStr(args, 3), ExecutedAt: argStr(args, 4), ExecutionTimeMs: argInt64(args, 5),
			Success: 0, ErrorMessage: argStr(args, 6), DownSQL: argStr(args, 7), DownHash: argStr(args, 8),
		})
		return fakeResult{rows: 1}, nil
	default:
		// Any other statement is a migration up/down body — record it so tests
		// can assert what ran and in which order.
		if err, fail := s.failBodies[query]; fail {
			return nil, err
		}
		s.execLog = append(s.execLog, query)
		return fakeResult{rows: 1}, nil
	}
}

func (s *fakeStore) query(query string, args []driver.NamedValue) (driver.Rows, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.queryErr != nil {
		return nil, s.queryErr
	}
	switch {
	case query == listSuccessSQL || query == listAllSQL:
		dbName := argStr(args, 0)
		successOnly := query == listSuccessSQL
		var matched []Migration
		for _, m := range s.rows {
			if m.DBName == dbName && (!successOnly || m.Success == 1) {
				matched = append(matched, m)
			}
		}
		sort.SliceStable(matched, func(i, j int) bool {
			if matched[i].ExecutedAt != matched[j].ExecutedAt {
				return matched[i].ExecutedAt < matched[j].ExecutedAt
			}
			return matched[i].Name < matched[j].Name
		})
		return migrationRows(matched), nil
	case strings.Contains(query, "DELETE FROM migration.migrations"):
		id := argStr(args, 0)
		cols := []string{"id"}
		if s.deleteLocked(id) {
			return &fakeRows{cols: cols, data: [][]driver.Value{{id}}}, nil
		}
		return &fakeRows{cols: cols}, nil // no row → sql.ErrNoRows on Scan
	default:
		return &fakeRows{}, nil
	}
}

// put inserts or replaces a row by ID (mirrors ON CONFLICT (id) DO UPDATE).
func (s *fakeStore) put(m Migration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.putLocked(m)
}

func (s *fakeStore) putLocked(m Migration) {
	for i := range s.rows {
		if s.rows[i].ID == m.ID {
			s.rows[i] = m
			return
		}
	}
	s.rows = append(s.rows, m)
}

func (s *fakeStore) deleteLocked(id string) bool {
	for i, m := range s.rows {
		if m.ID == id && m.Success == 1 {
			s.rows = append(s.rows[:i], s.rows[i+1:]...)
			return true
		}
	}
	return false
}

// count returns the number of applied (success) rows. The tests use a single
// datasource, so no per-datasource filter is needed.
func (s *fakeStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, m := range s.rows {
		if m.Success == 1 {
			n++
		}
	}
	return n
}

func (s *fakeStore) bodyLog() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.execLog...)
}

func (s *fakeStore) resetLog() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.execLog = nil
	s.statementLog = nil
	s.searchPaths = nil
	s.timeoutClears = 0
}

func (s *fakeStore) statements() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.statementLog...)
}

func (s *fakeStore) failQueries(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queryErr = err
}

func (s *fakeStore) timeoutClearCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.timeoutClears
}

func (s *fakeStore) searchPathLog() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.searchPaths...)
}

func migrationRows(ms []Migration) *fakeRows {
	cols := []string{
		"id", "db_name", "name", "hash", "executed_at",
		"execution_time_ms", "success", "error_message", "down_sql", "down_hash",
	}
	data := make([][]driver.Value, 0, len(ms))
	for _, m := range ms {
		data = append(data, []driver.Value{
			m.ID, m.DBName, m.Name, m.Hash, m.ExecutedAt,
			m.ExecutionTimeMs, int64(m.Success),
			nullOrStr(m.ErrorMessage), nullOrStr(m.DownSQL), nullOrStr(m.DownHash),
		})
	}
	return &fakeRows{cols: cols, data: data}
}

func nullOrStr(s string) driver.Value {
	if s == "" {
		return nil
	}
	return s
}

func argStr(args []driver.NamedValue, i int) string {
	if i >= 0 && i < len(args) {
		if v, ok := args[i].Value.(string); ok {
			return v
		}
	}
	return ""
}

func argInt64(args []driver.NamedValue, i int) int64 {
	if i >= 0 && i < len(args) {
		if v, ok := args[i].Value.(int64); ok {
			return v
		}
	}
	return 0
}

// --- driver.* plumbing ------------------------------------------------------

type fakeConn struct{ store *fakeStore }

var (
	_ driver.ExecerContext  = (*fakeConn)(nil)
	_ driver.QueryerContext = (*fakeConn)(nil)
	_ driver.ConnBeginTx    = (*fakeConn)(nil)
)

func (c *fakeConn) Prepare(string) (driver.Stmt, error) {
	return nil, io.ErrUnexpectedEOF // unused: ExecerContext/QueryerContext are implemented
}
func (c *fakeConn) Close() error              { return nil }
func (c *fakeConn) Begin() (driver.Tx, error) { return fakeTx{}, nil }
func (c *fakeConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return fakeTx{}, nil
}
func (c *fakeConn) ExecContext(_ context.Context, q string, a []driver.NamedValue) (driver.Result, error) {
	return c.store.exec(q, a)
}
func (c *fakeConn) QueryContext(_ context.Context, q string, a []driver.NamedValue) (driver.Rows, error) {
	return c.store.query(q, a)
}

type fakeTx struct{}

func (fakeTx) Commit() error   { return nil }
func (fakeTx) Rollback() error { return nil }

type fakeResult struct{ rows int64 }

func (fakeResult) LastInsertId() (int64, error)   { return 0, nil }
func (r fakeResult) RowsAffected() (int64, error) { return r.rows, nil }

type fakeRows struct {
	cols []string
	data [][]driver.Value
	pos  int
}

func (r *fakeRows) Columns() []string { return r.cols }
func (r *fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if r.pos >= len(r.data) {
		return io.EOF
	}
	copy(dest, r.data[r.pos])
	r.pos++
	return nil
}

type fakeConnector struct{ store *fakeStore }

func (c *fakeConnector) Connect(context.Context) (driver.Conn, error) {
	return &fakeConn{store: c.store}, nil
}
func (c *fakeConnector) Driver() driver.Driver { return fakeDriver{} }

type fakeDriver struct{}

func (fakeDriver) Open(string) (driver.Conn, error) {
	return nil, io.ErrUnexpectedEOF // open via OpenDB(connector) only
}

func newFakeMigratorDB(t *testing.T) (*stdsql.DB, *fakeStore) {
	t.Helper()
	store := &fakeStore{}
	db := stdsql.OpenDB(&fakeConnector{store: store})
	t.Cleanup(func() { _ = db.Close() })
	return db, store
}

// --- success-path tests -----------------------------------------------------

func TestMigrator_Up_AppliesAllInOrderThenIdempotent(t *testing.T) {
	spectest.Proves(t, "go/sql-migration-execution", "deterministic-execution", "definitions-execute-in-canonical-name-order")
	spectest.Proves(t, "go/sql-migration-execution", "repeatability-and-drift", "re-applying-a-matching-set-is-idempotent")
	db, store := newFakeMigratorDB(t)
	// Intentionally registered out of order to prove lexicographic ordering.
	m := NewMigrator(db, MigrationConfig{
		Datasource: "primary",
		Definitions: []Definition{
			{Name: "iam/003", SQL: "CREATE TABLE c (id int)"},
			{Name: "iam/001", SQL: "CREATE TABLE a (id int)"},
			{Name: "iam/002", SQL: "CREATE TABLE b (id int)"},
		},
	})

	applied, err := m.Up(context.Background())
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	if len(applied) != 3 {
		t.Fatalf("applied %d migrations, want 3", len(applied))
	}
	wantOrder := []string{"iam/001", "iam/002", "iam/003"}
	for i, w := range wantOrder {
		if applied[i].Name != w {
			t.Errorf("applied[%d].Name = %q, want %q", i, applied[i].Name, w)
		}
	}
	// Up bodies must have executed in lexicographic order.
	wantBodies := []string{"CREATE TABLE a (id int)", "CREATE TABLE b (id int)", "CREATE TABLE c (id int)"}
	assertStrings(t, "up body order", store.bodyLog(), wantBodies)
	if store.count() != 3 {
		t.Errorf("state-store rows = %d, want 3", store.count())
	}

	// Re-run: every migration is already applied — a no-op (idempotency skip).
	store.resetLog()
	again, err := m.Up(context.Background())
	if err != nil {
		t.Fatalf("second Up: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("second Up applied %d migrations, want 0 (idempotent)", len(again))
	}
	if bodies := store.bodyLog(); len(bodies) != 0 {
		t.Errorf("second Up executed bodies %v, want none", bodies)
	}
}

func TestMigrator_UpTo_StopsAtTarget(t *testing.T) {
	db, store := newFakeMigratorDB(t)
	m := NewMigrator(db, MigrationConfig{
		Datasource: "primary",
		Definitions: []Definition{
			{Name: "iam/001", SQL: "A"}, {Name: "iam/002", SQL: "B"}, {Name: "iam/003", SQL: "C"},
		},
	})
	applied, err := m.UpTo(context.Background(), "iam/002")
	if err != nil {
		t.Fatalf("UpTo: %v", err)
	}
	if len(applied) != 2 || applied[0].Name != "iam/001" || applied[1].Name != "iam/002" {
		t.Fatalf("UpTo applied %v, want through iam/002", names(applied))
	}
	if store.count() != 2 {
		t.Errorf("state-store rows = %d, want 2", store.count())
	}
}

func TestMigrator_Rollback_RunsDownAndRemovesRow(t *testing.T) {
	spectest.Proves(t, "go/sql-migration-execution", "reversibility", "rollback-runs-only-the-persisted-down-body")
	db, store := newFakeMigratorDB(t)
	m := NewMigrator(db, MigrationConfig{
		Datasource:  "primary",
		Definitions: []Definition{{Name: "iam/001", SQL: "CREATE TABLE a (id int)", Down: "DROP TABLE a"}},
	})
	if _, err := m.Up(context.Background()); err != nil {
		t.Fatalf("Up: %v", err)
	}
	store.resetLog()

	rec, err := m.Rollback(context.Background())
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if rec == nil || rec.Name != "iam/001" {
		t.Fatalf("rolled back %+v, want iam/001", rec)
	}
	if store.count() != 0 {
		t.Errorf("rows after Rollback = %d, want 0 (row must be removed)", store.count())
	}
	assertStrings(t, "down body", store.bodyLog(), []string{"DROP TABLE a"})
}

func TestMigrator_Rollback_RejectsTamperedDownSQL(t *testing.T) {
	spectest.Proves(t, "go/sql-migration-execution", "reversibility", "a-tampered-down-body-is-refused")
	db, store := newFakeMigratorDB(t)
	m := NewMigrator(db, MigrationConfig{
		Datasource:  "primary",
		Definitions: []Definition{{Name: "iam/001", SQL: "CREATE TABLE a (id int)", Down: "DROP TABLE a"}},
	})
	// An applied row whose stored down_sql no longer matches its down_hash:
	// the integrity check must refuse to run it.
	store.put(Migration{
		ID: "primary:iam/001", DBName: "primary", Name: "iam/001",
		Hash: sha256Hex("CREATE TABLE a (id int)"), ExecutedAt: "2026-06-01T00:00:00Z",
		Success: 1, DownSQL: "DROP TABLE attacker", DownHash: sha256Hex("DROP TABLE a"),
	})

	_, err := m.Rollback(context.Background())
	if err == nil {
		t.Fatal("expected a rollback integrity error for tampered down_sql")
	}
	if !strings.Contains(err.Error(), "integrity") {
		t.Errorf("error = %v, want an integrity-check failure", err)
	}
	if store.count() != 1 {
		t.Errorf("tampered row must not be removed, count = %d", store.count())
	}
	if len(store.bodyLog()) != 0 {
		t.Errorf("no down body should run for a tampered migration, got %v", store.bodyLog())
	}
}

func TestMigrator_RollbackTo_Reset_AndStatus(t *testing.T) {
	db, store := newFakeMigratorDB(t)
	defs := []Definition{
		{Name: "iam/001", SQL: "A", Down: "dA"},
		{Name: "iam/002", SQL: "B", Down: "dB"},
		{Name: "iam/003", SQL: "C", Down: "dC"},
	}
	m := NewMigrator(db, MigrationConfig{Datasource: "primary", Definitions: defs})
	if _, err := m.Up(context.Background()); err != nil {
		t.Fatalf("Up: %v", err)
	}
	store.resetLog()

	st, err := m.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(st) != 3 {
		t.Fatalf("Status returned %d rows, want 3", len(st))
	}
	if got := store.statements(); len(got) != 0 {
		t.Fatalf("Status executed mutating statements %v, want none", got)
	}

	// RollbackTo iam/001 rolls back everything applied after it.
	rolled, err := m.RollbackTo(context.Background(), "iam/001")
	if err != nil {
		t.Fatalf("RollbackTo: %v", err)
	}
	if len(rolled) != 2 {
		t.Fatalf("RollbackTo rolled %d, want 2", len(rolled))
	}
	if store.count() != 1 {
		t.Errorf("rows after RollbackTo = %d, want 1 (iam/001 kept)", store.count())
	}

	// Reset rolls back the remainder.
	if _, err := m.Reset(context.Background()); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if store.count() != 0 {
		t.Errorf("rows after Reset = %d, want 0", store.count())
	}
}

func TestMigrator_Status_MissingStateStoreIsEmptyAndReadOnly(t *testing.T) {
	spectest.Proves(t, "go/sql-migration-execution", "read-only-observation", "status-with-a-runtime-role-never-creates-or-locks-the-state-store")

	for _, sqlState := range []string{sqlStateUndefinedTable, sqlStateInvalidSchemaName} {
		t.Run(sqlState, func(t *testing.T) {
			db, store := newFakeMigratorDB(t)
			store.failQueries(&pgconn.PgError{Code: sqlState})
			m := NewMigrator(db, MigrationConfig{
				Datasource:  "primary",
				Definitions: []Definition{{Name: "iam/001", SQL: "A"}},
			})

			rows, err := m.Status(context.Background())
			if err != nil {
				t.Fatalf("Status: %v", err)
			}
			if len(rows) != 0 {
				t.Fatalf("Status returned %d rows, want empty missing store", len(rows))
			}
			if got := store.statements(); len(got) != 0 {
				t.Fatalf("Status executed mutating statements %v, want none", got)
			}
		})
	}
}

func TestMigrator_Status_PropagatesOtherReadFailureWithoutWriting(t *testing.T) {
	db, store := newFakeMigratorDB(t)
	wantErr := errors.New("connection interrupted")
	store.failQueries(wantErr)
	m := NewMigrator(db, MigrationConfig{Datasource: "primary"})

	_, err := m.Status(context.Background())
	if !errors.Is(err, wantErr) {
		t.Fatalf("Status error = %v, want wrapped %v", err, wantErr)
	}
	if got := store.statements(); len(got) != 0 {
		t.Fatalf("Status executed mutating statements %v after read failure", got)
	}
}

func TestMigrator_Verify_NoDriftAfterUp(t *testing.T) {
	spectest.Proves(t, "go/sql-migration-execution", "repeatability-and-drift", "a-clean-up-reports-no-drift")
	db, store := newFakeMigratorDB(t)
	m := NewMigrator(db, MigrationConfig{
		Datasource:  "primary",
		Definitions: []Definition{{Name: "iam/001", SQL: "A"}, {Name: "iam/002", SQL: "B"}},
	})
	if _, err := m.Up(context.Background()); err != nil {
		t.Fatalf("Up: %v", err)
	}
	store.resetLog()
	report, err := m.Verify(context.Background())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(report.HashDrifts) != 0 || len(report.MissingFromStore) != 0 || len(report.MissingFromRegistry) != 0 {
		t.Errorf("expected no drift after a clean Up, got %+v", report)
	}
	if got := store.statements(); len(got) != 0 {
		t.Fatalf("Verify executed mutating statements %v, want none", got)
	}
}

// --- search_path tests ------------------------------------------------------

func TestMigrator_Up_SetsTransactionLocalSearchPathFromSchema(t *testing.T) {
	db, store := newFakeMigratorDB(t)
	m := NewMigrator(db, MigrationConfig{
		Datasource: "registry_put",
		Schema:     "registry_put",
		Definitions: []Definition{
			{Name: "registry/001", SQL: "CREATE TABLE blobs (id int)"},
			{Name: "registry/002", SQL: "CREATE TABLE manifests (id int)"},
		},
	})
	if _, err := m.Up(context.Background()); err != nil {
		t.Fatalf("Up: %v", err)
	}
	// The declared schema is set as search_path once per applied migration so
	// each body's unqualified DDL lands in it.
	assertStrings(t, "search_path per migration", store.searchPathLog(),
		[]string{"registry_put", "registry_put"})
	// And the set_config statement must never leak into the recorded bodies.
	assertStrings(t, "bodies", store.bodyLog(),
		[]string{"CREATE TABLE blobs (id int)", "CREATE TABLE manifests (id int)"})
}

func TestMigrator_Up_ClearsStatementTimeoutPerMigration(t *testing.T) {
	db, store := newFakeMigratorDB(t)
	m := NewMigrator(db, MigrationConfig{
		Datasource: "registry_put",
		Schema:     "registry_put",
		Definitions: []Definition{
			{Name: "registry/001", SQL: "CREATE INDEX CONCURRENTLY big ON t (a)"},
			{Name: "registry/002", SQL: "CREATE TABLE manifests (id int)"},
		},
	})
	if _, err := m.Up(context.Background()); err != nil {
		t.Fatalf("Up: %v", err)
	}
	// Each applied migration's transaction disables the pool's statement_timeout
	// so a long DDL statement is not aborted mid-migration.
	if got := store.timeoutClearCount(); got != 2 {
		t.Errorf("statement_timeout clears = %d, want 2 (one per applied migration)", got)
	}
	// The set_config statement must not leak into the recorded bodies.
	assertStrings(t, "bodies", store.bodyLog(),
		[]string{"CREATE INDEX CONCURRENTLY big ON t (a)", "CREATE TABLE manifests (id int)"})
}

func TestMigrator_Up_NoSchema_LeavesConnectionSearchPath(t *testing.T) {
	db, store := newFakeMigratorDB(t)
	m := NewMigrator(db, MigrationConfig{
		Datasource:  "primary",
		Definitions: []Definition{{Name: "iam/001", SQL: "CREATE TABLE a (id int)"}},
	})
	if _, err := m.Up(context.Background()); err != nil {
		t.Fatalf("Up: %v", err)
	}
	// With no declared schema the runner sets no search_path — the connection's
	// own (pool/binding) search_path stays in force, the path the published
	// bundle and the test provider rely on.
	if sp := store.searchPathLog(); len(sp) != 0 {
		t.Errorf("no schema declared, but search_path was set: %v", sp)
	}
}

func TestMigrator_Rollback_SetsSearchPathForDownBody(t *testing.T) {
	db, store := newFakeMigratorDB(t)
	m := NewMigrator(db, MigrationConfig{
		Datasource:  "registry_put",
		Schema:      "registry_put",
		Definitions: []Definition{{Name: "registry/001", SQL: "CREATE TABLE a (id int)", Down: "DROP TABLE a"}},
	})
	if _, err := m.Up(context.Background()); err != nil {
		t.Fatalf("Up: %v", err)
	}
	store.resetLog() // isolate the rollback's statements from the up

	if _, err := m.Rollback(context.Background()); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	assertStrings(t, "rollback search_path", store.searchPathLog(), []string{"registry_put"})
	assertStrings(t, "down body", store.bodyLog(), []string{"DROP TABLE a"})
}

// --- helpers ----------------------------------------------------------------

func assertStrings(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %v, want %v", label, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s[%d] = %q, want %q (full: %v)", label, i, got[i], want[i], got)
		}
	}
}

func names(ms []Migration) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.Name
	}
	return out
}
