package database

import (
	"go.putnami.dev/protocol/features/spectest"

	"context"
	stdsql "database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"testing"

	perrors "go.putnami.dev/errors"
	"go.putnami.dev/migration"
)

// --- test driver: minimal sql driver for migration error-path testing ------

type testMigratorDriver struct{}
type testMigratorConn struct{}

func init() {
	stdsql.Register("test_migrator", &testMigratorDriver{})
}

func (d *testMigratorDriver) Open(_ string) (driver.Conn, error) {
	return &testMigratorConn{}, nil
}

func (c *testMigratorConn) Prepare(_ string) (driver.Stmt, error) {
	return nil, fmt.Errorf("test: prepare not supported")
}
func (c *testMigratorConn) Close() error { return nil }
func (c *testMigratorConn) Begin() (driver.Tx, error) {
	return nil, fmt.Errorf("test: begin not supported")
}

// newTestMigratorDB returns a *stdsql.DB backed by the test driver.
func newTestMigratorDB(t *testing.T) *stdsql.DB {
	t.Helper()
	db, err := stdsql.Open("test_migrator", "test")
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// assertProtocolMigrationError checks that err is a structured error
// with one of the canonical migration.* protocol codes.
func assertProtocolMigrationError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var ce *perrors.Error
	if !errors.As(err, &ce) {
		t.Fatalf("expected *perrors.Error, got %T: %v", err, err)
	}
	code := string(ce.Code())
	if !strings.HasPrefix(code, "migration.") {
		t.Errorf("expected a migration.* protocol code, got %q", code)
	}
}

// --- Migrator construction -------------------------------------------------

func TestNewMigrator_DefaultDatasource_Empty(t *testing.T) {
	m := NewMigrator(nil, MigrationConfig{})
	if got := m.Datasource(); got != "default" {
		t.Errorf("expected canonical default datasource, got %q", got)
	}
	if len(m.Definitions()) != 0 {
		t.Errorf("expected no definitions")
	}
}

func TestNewMigrator_SortsDefinitionsByName(t *testing.T) {
	m := NewMigrator(nil, MigrationConfig{
		Definitions: []Definition{
			{Name: "iam/003", SQL: "X"},
			{Name: "iam/001", SQL: "X"},
			{Name: "iam/002", SQL: "X"},
		},
	})
	defs := m.Definitions()
	if !(defs[0].Name == "iam/001" && defs[1].Name == "iam/002" && defs[2].Name == "iam/003") {
		t.Fatalf("definitions not sorted by Name: %v", defs)
	}
}

func TestNewMigrator_DefinitionsAreCopied(t *testing.T) {
	source := []Definition{{Name: "iam/001", SQL: "X"}}
	m := NewMigrator(nil, MigrationConfig{Definitions: source})
	source[0].Name = "MUTATED"
	if m.Definitions()[0].Name != "iam/001" {
		t.Fatal("Migrator must copy its Definitions input")
	}
}

// --- Migrator.Up edge paths ------------------------------------------------

func TestNewMigrator_NilDB_FailsValidation(t *testing.T) {
	m := NewMigrator(nil, MigrationConfig{
		Definitions: []Definition{{Name: "iam/001", SQL: "SELECT 1"}},
	})
	_, err := m.Up(context.Background())
	assertProtocolMigrationError(t, err)
}

func TestMigrator_Up_NoDefinitions_ReturnsNil(t *testing.T) {
	db := newTestMigratorDB(t)
	m := NewMigrator(db, MigrationConfig{Datasource: "primary"})
	got, err := m.Up(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil result, got %v", got)
	}
}

func TestMigrator_Up_DBError(t *testing.T) {
	db := newTestMigratorDB(t)
	m := NewMigrator(db, MigrationConfig{
		Definitions: []Definition{{Name: "iam/001", SQL: "SELECT 1"}},
	})
	_, err := m.Up(context.Background())
	assertProtocolMigrationError(t, err)
}

func TestMigrator_Up_CanceledContext(t *testing.T) {
	db := newTestMigratorDB(t)
	m := NewMigrator(db, MigrationConfig{
		Definitions: []Definition{{Name: "iam/001", SQL: "SELECT 1"}},
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := m.Up(ctx)
	assertProtocolMigrationError(t, err)
}

func TestMigrator_Status_DBError(t *testing.T) {
	db := newTestMigratorDB(t)
	m := NewMigrator(db, MigrationConfig{})
	_, err := m.Status(context.Background())
	assertProtocolMigrationError(t, err)
}

func TestMigrator_Rollback_DBError(t *testing.T) {
	db := newTestMigratorDB(t)
	m := NewMigrator(db, MigrationConfig{})
	_, err := m.Rollback(context.Background())
	assertProtocolMigrationError(t, err)
}

func TestMigrator_RollbackTo_DBError(t *testing.T) {
	db := newTestMigratorDB(t)
	m := NewMigrator(db, MigrationConfig{
		Definitions: []Definition{{Name: "iam/001", SQL: "X"}},
	})
	_, err := m.RollbackTo(context.Background(), "iam/001")
	assertProtocolMigrationError(t, err)
}

func TestMigrator_Reset_DBError(t *testing.T) {
	db := newTestMigratorDB(t)
	m := NewMigrator(db, MigrationConfig{})
	_, err := m.Reset(context.Background())
	assertProtocolMigrationError(t, err)
}

// --- Name resolution -------------------------------------------------------

func TestMigrator_ResolveName_FullForm(t *testing.T) {
	m := NewMigrator(nil, MigrationConfig{
		Definitions: []Definition{
			{Name: "iam/001_init"},
			{Name: "secrets/001_init"},
		},
	})
	got, err := m.resolveName("iam/001_init")
	if err != nil {
		t.Fatalf("resolveName: %v", err)
	}
	if got != "iam/001_init" {
		t.Errorf("got %q, want iam/001_init", got)
	}
}

func TestMigrator_ResolveName_BaseNameUnique(t *testing.T) {
	m := NewMigrator(nil, MigrationConfig{
		Definitions: []Definition{
			{Name: "iam/001_create_users"},
			{Name: "iam/002_add_index"},
		},
	})
	got, err := m.resolveName("002_add_index")
	if err != nil {
		t.Fatalf("resolveName: %v", err)
	}
	if got != "iam/002_add_index" {
		t.Errorf("got %q, want iam/002_add_index", got)
	}
}

func TestMigrator_ResolveName_BaseNameAmbiguous(t *testing.T) {
	m := NewMigrator(nil, MigrationConfig{
		Definitions: []Definition{
			{Name: "iam/001_init"},
			{Name: "secrets/001_init"},
		},
	})
	_, err := m.resolveName("001_init")
	if err == nil {
		t.Fatal("expected ambiguity error")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("error must mention ambiguity, got %v", err)
	}
}

func TestMigrator_ResolveName_NotFound(t *testing.T) {
	m := NewMigrator(nil, MigrationConfig{
		Definitions: []Definition{{Name: "iam/001_init"}},
	})
	if _, err := m.resolveName("unknown"); err == nil {
		t.Fatal("expected not-found error")
	}
}

// --- Record adaptation -----------------------------------------------------

func TestRecordFromMigration_RoundTripsStatusAndTime(t *testing.T) {
	m := Migration{
		ID: "default:iam/001", DBName: "default", Name: "iam/001",
		Hash: "abc", ExecutedAt: "2026-05-22T12:00:00Z",
		ExecutionTimeMs: 42, Success: 1,
	}
	rec := recordFromMigration(m, "iam", "default", "embed:iam/migrations/001.up.sql")
	if rec.Status != migration.StatusApplied {
		t.Errorf("expected applied, got %s", rec.Status)
	}
	if rec.Kind != migration.KindSQL {
		t.Errorf("expected sql, got %s", rec.Kind)
	}
	if rec.ExecutedAt.IsZero() {
		t.Error("expected non-zero ExecutedAt")
	}
	if rec.Target != "default" {
		t.Errorf("Target not set: %s", rec.Target)
	}
	if rec.Namespace != "iam" {
		t.Errorf("Namespace not propagated: %s", rec.Namespace)
	}
}

func TestRecordFromMigration_FailureMapsToStatusFailed(t *testing.T) {
	m := Migration{Success: 0, Name: "iam/001", ErrorMessage: "boom"}
	rec := recordFromMigration(m, "iam", "default", "")
	if rec.Status != migration.StatusFailed {
		t.Errorf("expected failed, got %s", rec.Status)
	}
	if rec.Error != "boom" {
		t.Errorf("Error not propagated: %s", rec.Error)
	}
}

// --- Utilities -------------------------------------------------------------

func TestSha256Hex_KnownVectors(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
		{"hello", "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"},
	}
	for _, c := range cases {
		if got := sha256Hex(c.in); got != c.want {
			t.Errorf("sha256Hex(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSha256Hex_Deterministic(t *testing.T) {
	sql := "CREATE TABLE users (id TEXT PRIMARY KEY);"
	a := sha256Hex(sql)
	b := sha256Hex(sql)
	if a != b {
		t.Fatal("expected deterministic hash")
	}
}

func TestAdvisoryLockKey_ScopedPerDatasource(t *testing.T) {
	spectest.Proves(t, "go/sql-migration-execution", "atomic-record", "the-advisory-lock-is-scoped-per-datasource")
	if got := advisoryLockKey("svc"); got != "putnami.migration:svc" {
		t.Errorf("got %q, want putnami.migration:svc", got)
	}
	if advisoryLockKey("default") == advisoryLockKey("other") {
		t.Error("expected distinct keys for distinct datasources")
	}
}

func TestResolveDatasource_Priority(t *testing.T) {
	cases := []struct{ primary, fallback, want string }{
		{"", "", "default"},
		{"", "fb", "fb"},
		{"pri", "", "pri"},
		{"pri", "fb", "pri"},
	}
	for _, c := range cases {
		if got := resolveDatasource(c.primary, c.fallback); got != c.want {
			t.Errorf("resolveDatasource(%q,%q) = %q, want %q",
				c.primary, c.fallback, got, c.want)
		}
	}
}

func TestNullableString(t *testing.T) {
	if got := nullableString(""); got != nil {
		t.Errorf("empty should resolve to nil, got %v", got)
	}
	if got := nullableString("x"); got != "x" {
		t.Errorf("non-empty should pass through, got %v", got)
	}
}

func TestSortByExecutedAtDesc(t *testing.T) {
	records := []Migration{
		{Name: "a", ExecutedAt: "2026-01-01T00:00:00Z"},
		{Name: "b", ExecutedAt: "2026-01-03T00:00:00Z"},
		{Name: "c", ExecutedAt: "2026-01-02T00:00:00Z"},
	}
	sortByExecutedAtDesc(records)
	if records[0].Name != "b" || records[1].Name != "c" || records[2].Name != "a" {
		t.Errorf("got %v", records)
	}
}
