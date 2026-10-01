package database

import (
	"go.putnami.dev/protocol/features/spectest"

	"context"
	stdsql "database/sql"
	"database/sql/driver"
	"errors"
	"sync"
	"testing"
)

// The atomic-record requirement promises that a migration body and its success
// state-store record commit in ONE transaction: a crash between the two can
// never leave an applied body without its record, or a record without its
// body. The shared fake proves ordering and content but is blind to
// transaction boundaries, so this file wraps it with a connection that stamps
// every statement with the generation of the transaction it ran under.

type txTrace struct {
	mu         sync.Mutex
	gen        int
	openGen    int // 0 = outside any transaction
	execs      []tracedExec
	committed  map[int]bool
	rolledBack map[int]bool
}

type tracedExec struct {
	gen   int
	query string
}

func newTxTrace() *txTrace {
	return &txTrace{committed: map[int]bool{}, rolledBack: map[int]bool{}}
}

func (tr *txTrace) genOf(query string) (int, bool) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	for _, e := range tr.execs {
		if e.query == query {
			return e.gen, true
		}
	}
	return 0, false
}

type tracingConn struct {
	*fakeConn
	trace *txTrace
}

func (c *tracingConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	c.trace.mu.Lock()
	c.trace.gen++
	c.trace.openGen = c.trace.gen
	g := c.trace.gen
	c.trace.mu.Unlock()
	return tracingTx{trace: c.trace, gen: g}, nil
}

func (c *tracingConn) ExecContext(ctx context.Context, q string, a []driver.NamedValue) (driver.Result, error) {
	c.trace.mu.Lock()
	c.trace.execs = append(c.trace.execs, tracedExec{gen: c.trace.openGen, query: q})
	c.trace.mu.Unlock()
	return c.fakeConn.ExecContext(ctx, q, a)
}

type tracingTx struct {
	trace *txTrace
	gen   int
}

func (t tracingTx) Commit() error {
	t.trace.mu.Lock()
	t.trace.committed[t.gen] = true
	t.trace.openGen = 0
	t.trace.mu.Unlock()
	return nil
}

func (t tracingTx) Rollback() error {
	t.trace.mu.Lock()
	t.trace.rolledBack[t.gen] = true
	t.trace.openGen = 0
	t.trace.mu.Unlock()
	return nil
}

type tracingConnector struct {
	store *fakeStore
	trace *txTrace
}

func (c *tracingConnector) Connect(context.Context) (driver.Conn, error) {
	return &tracingConn{fakeConn: &fakeConn{store: c.store}, trace: c.trace}, nil
}
func (c *tracingConnector) Driver() driver.Driver { return fakeDriver{} }

func TestMigrator_Up_BodyAndRecordCommitInOneTransaction(t *testing.T) {
	spectest.Proves(t, "go/sql-migration-execution", "atomic-record", "the-body-and-its-record-commit-in-one-transaction")
	trace := newTxTrace()
	store := &fakeStore{}
	db := stdsql.OpenDB(&tracingConnector{store: store, trace: trace})
	t.Cleanup(func() { _ = db.Close() })

	const body = "CREATE TABLE a (id int)"
	m := NewMigrator(db, MigrationConfig{
		Datasource:  "primary",
		Definitions: []Definition{{Name: "iam/001", SQL: body}},
	})
	if _, err := m.Up(context.Background()); err != nil {
		t.Fatalf("Up: %v", err)
	}

	bodyGen, ok := trace.genOf(body)
	if !ok || bodyGen == 0 {
		t.Fatalf("migration body ran outside a transaction (gen %d, seen %v)", bodyGen, ok)
	}
	recordGen, ok := trace.genOf(upsertSuccessSQL)
	if !ok || recordGen == 0 {
		t.Fatalf("success record ran outside a transaction (gen %d, seen %v)", recordGen, ok)
	}
	if bodyGen != recordGen {
		t.Fatalf("body committed under tx %d but its record under tx %d; the pair must share one transaction", bodyGen, recordGen)
	}
	if !trace.committed[bodyGen] {
		t.Fatalf("the body+record transaction %d never committed", bodyGen)
	}
	// The advisory lock is session-scoped, deliberately outside the transaction.
	if lockGen, ok := trace.genOf(acquireAdvisoryLockSQL); !ok || lockGen != 0 {
		t.Errorf("advisory lock acquired under tx %d; it must be session-scoped, outside the transaction", lockGen)
	}
}

func TestMigrator_Up_FailedBodyRollsBackAndIsNeverApplied(t *testing.T) {
	spectest.Proves(t, "go/sql-migration-execution", "atomic-record", "a-failed-body-rolls-back-and-is-never-applied")
	trace := newTxTrace()
	const body = "CREATE TABLE broken (id int)"
	store := &fakeStore{failBodies: map[string]error{body: errors.New("boom")}}
	db := stdsql.OpenDB(&tracingConnector{store: store, trace: trace})
	t.Cleanup(func() { _ = db.Close() })

	m := NewMigrator(db, MigrationConfig{
		Datasource:  "primary",
		Definitions: []Definition{{Name: "iam/001", SQL: body}},
	})
	applied, err := m.Up(context.Background())
	if err == nil {
		t.Fatal("Up succeeded over a failing body")
	}
	if len(applied) != 0 {
		t.Fatalf("a failed migration was returned as applied: %+v", applied)
	}

	bodyGen, _ := trace.genOf(body)
	if bodyGen == 0 {
		t.Fatal("failing body was not attempted inside a transaction")
	}
	if !trace.rolledBack[bodyGen] {
		t.Fatalf("transaction %d of the failed body did not roll back", bodyGen)
	}
	if _, ok := trace.genOf(upsertSuccessSQL); ok {
		t.Fatal("a success record was written for a failed migration")
	}
	// The failure record is bookkeeping on the session connection, outside the
	// rolled-back transaction — otherwise it would vanish with it.
	if failGen, ok := trace.genOf(upsertFailureSQL); ok && failGen != 0 {
		t.Errorf("failure record written under tx %d; it must survive the rollback", failGen)
	}
}

// The no-drift half of repeatability-and-drift is covered by
// TestMigrator_Verify_NoDriftAfterUp; this is the half that finds problems: a
// changed body, a definition the store knows but the registry lost, and a
// pending definition the store has never seen must each appear in the report.
func TestMigrator_Verify_ReportsChangedMissingAndPending(t *testing.T) {
	spectest.Proves(t, "go/sql-migration-execution", "repeatability-and-drift", "changed-missing-and-pending-definitions-appear-in-the-report")
	db, _ := newFakeMigratorDB(t)
	m := NewMigrator(db, MigrationConfig{
		Datasource:  "primary",
		Definitions: []Definition{{Name: "iam/001", SQL: "A"}, {Name: "iam/002", SQL: "B"}},
	})
	if _, err := m.Up(context.Background()); err != nil {
		t.Fatalf("Up: %v", err)
	}

	drifted := NewMigrator(db, MigrationConfig{
		Datasource: "primary",
		Definitions: []Definition{
			{Name: "iam/001", SQL: "A CHANGED"}, // hash drift
			// iam/002 dropped from the registry entirely
			{Name: "iam/003", SQL: "C"}, // pending, never applied
		},
	})
	report, err := drifted.Verify(context.Background())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(report.HashDrifts) != 1 {
		t.Errorf("hash drifts = %+v, want exactly iam/001", report.HashDrifts)
	}
	if len(report.MissingFromRegistry) != 1 {
		t.Errorf("missing-from-registry = %+v, want exactly iam/002", report.MissingFromRegistry)
	}
	if len(report.MissingFromStore) != 1 {
		t.Errorf("missing-from-store = %+v, want exactly iam/003", report.MissingFromStore)
	}
}

// Rollback must undo in reverse recorded execution order, or a later
// migration's down would run against state its predecessor already removed.
func TestMigrator_RollbackTo_RunsDownBodiesInReverseOrder(t *testing.T) {
	spectest.Proves(t, "go/sql-migration-execution", "deterministic-execution", "rollback-proceeds-in-reverse-recorded-order")
	db, store := newFakeMigratorDB(t)
	m := NewMigrator(db, MigrationConfig{
		Datasource: "primary",
		Definitions: []Definition{
			{Name: "iam/001", SQL: "UP1", Down: "DOWN1"},
			{Name: "iam/002", SQL: "UP2", Down: "DOWN2"},
			{Name: "iam/003", SQL: "UP3", Down: "DOWN3"},
		},
	})
	if _, err := m.Up(context.Background()); err != nil {
		t.Fatalf("Up: %v", err)
	}
	rolled, err := m.RollbackTo(context.Background(), "iam/001")
	if err != nil {
		t.Fatalf("RollbackTo: %v", err)
	}
	if len(rolled) != 2 {
		t.Fatalf("rolled back %d migrations, want 2 (003 then 002)", len(rolled))
	}
	if rolled[0].Name != "iam/003" || rolled[1].Name != "iam/002" {
		t.Errorf("rollback order = [%s %s], want [iam/003 iam/002]", rolled[0].Name, rolled[1].Name)
	}
	bodies := store.bodyLog()
	if len(bodies) < 2 || bodies[len(bodies)-2] != "DOWN3" || bodies[len(bodies)-1] != "DOWN2" {
		t.Errorf("down bodies ran as %v, want …DOWN3 then DOWN2", bodies)
	}
}

// Rollback must refuse a migration that persisted no down body rather than
// guessing destructive SQL. migration.go refuses at the call site; nothing
// proved it.
func TestMigrator_Rollback_RefusesAMissingDownBody(t *testing.T) {
	spectest.Proves(t, "go/sql-migration-execution", "reversibility", "a-missing-down-body-is-refused")
	db, _ := newFakeMigratorDB(t)
	m := NewMigrator(db, MigrationConfig{
		Datasource:  "primary",
		Definitions: []Definition{{Name: "iam/001", SQL: "UP1"}}, // no Down
	})
	if _, err := m.Up(context.Background()); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if _, err := m.Rollback(context.Background()); err == nil {
		t.Fatal("Rollback succeeded with no persisted down body")
	}
}
