package database

import (
	"go.putnami.dev/protocol/features/spectest"

	"context"
	stdsql "database/sql"
	stderrors "errors"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5/pgconn"

	perrors "go.putnami.dev/errors"
	"go.putnami.dev/migration"
)

// --- Helpers ----------------------------------------------------------------

// stubOpenDB returns a thunk that yields the test SQL driver db each call.
func stubOpenDB(t *testing.T) func() (*stdsql.DB, error) {
	t.Helper()
	db := newTestMigratorDB(t)
	return func() (*stdsql.DB, error) { return db, nil }
}

// --- Apply gating -----------------------------------------------------------

func TestSQLRunner_Apply_NoOpWhenAutoApplyOffAndNotForced(t *testing.T) {
	reg := migration.NewRegistry()
	runner := newSQLRunner(SQLRunnerOptions{
		Registry:  reg,
		OpenDB:    func() (*stdsql.DB, error) { t.Fatal("OpenDB must not be called"); return nil, nil },
		AutoApply: false,
	})
	records, err := runner.Apply(context.Background(), migration.ApplyOpts{Force: false})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if records != nil {
		t.Fatalf("expected nil records, got %v", records)
	}
}

func TestSQLRunner_Apply_NoSourcesIsNoop(t *testing.T) {
	reg := migration.NewRegistry()
	runner := newSQLRunner(SQLRunnerOptions{
		Registry:  reg,
		OpenDB:    stubOpenDB(t),
		AutoApply: true,
	})
	records, err := runner.Apply(context.Background(), migration.ApplyOpts{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if records != nil {
		t.Fatalf("expected nil records, got %v", records)
	}
}

func TestSQLRunner_Apply_ForceOverridesAutoApply(t *testing.T) {
	reg := migration.NewRegistry()
	// AutoApply: false but Force: true — runner must attempt the apply,
	// reaching the test driver and surfacing its protocol error.
	_ = reg.AddSource(NewSQLSource("iam", Datasource{Name: "default"}, fstest.MapFS{
		"001_init.up.sql": &fstest.MapFile{Data: []byte("SELECT 1")},
	}))
	runner := newSQLRunner(SQLRunnerOptions{
		Registry:  reg,
		OpenDB:    stubOpenDB(t),
		AutoApply: false,
	})
	_, err := runner.Apply(context.Background(), migration.ApplyOpts{Force: true})
	if err == nil {
		t.Fatal("expected protocol error from test driver")
	}
	var pe *perrors.Error
	if !stderrors.As(err, &pe) {
		t.Fatalf("expected *perrors.Error, got %T", err)
	}
}

// --- Source materialization -------------------------------------------------

func TestSQLRunner_NonSQLSource_ReportsInvalidDef(t *testing.T) {
	reg := migration.NewRegistry()
	// Register a source under Kind=sql that is NOT an SQLSource. The
	// only public way to do this is via a custom type that lies about
	// its Kind — exercise the defensive cast in materializeMigrators.
	_ = reg.AddSource(fakeNonSQLSource{ns: "rogue"})

	runner := newSQLRunner(SQLRunnerOptions{
		Registry:  reg,
		OpenDB:    stubOpenDB(t),
		AutoApply: true,
	})
	_, err := runner.Apply(context.Background(), migration.ApplyOpts{Force: true})
	if err == nil {
		t.Fatal("expected error on non-SQLSource contribution")
	}
	var pe *perrors.Error
	if !stderrors.As(err, &pe) || pe.Code() != CodeMigrationInvalidDef {
		t.Fatalf("expected CodeMigrationInvalidDef, got %v", err)
	}
}

func TestSQLRunner_DuplicateNameAcrossSources_ReportsConflict(t *testing.T) {
	reg := migration.NewRegistry()
	_ = reg.AddSource(NewSQLSource("iam", Datasource{Name: "default"}, fstest.MapFS{
		"001_init.up.sql": &fstest.MapFile{Data: []byte("X")},
	}))
	// Second source contributes the same namespaced name via inline.
	_ = reg.AddSource(NewSQLSource("iam", Datasource{Name: "default"}, nil,
		Definition{Name: "iam/001_init", SQL: "Y"},
	))

	runner := newSQLRunner(SQLRunnerOptions{
		Registry:  reg,
		OpenDB:    stubOpenDB(t),
		AutoApply: true,
	})
	_, err := runner.Apply(context.Background(), migration.ApplyOpts{Force: true})
	if err == nil {
		t.Fatal("expected duplicate-name error")
	}
	var pe *perrors.Error
	if !stderrors.As(err, &pe) || pe.Code() != CodeMigrationInvalidDef {
		t.Fatalf("expected CodeMigrationInvalidDef, got %v", err)
	}
}

func TestSQLRunner_ApplyTo_BasenameAmbiguousAcrossNamespaces(t *testing.T) {
	// Two namespaces on the SAME datasource contributing the same
	// basename (001_init). When the CLI passes opts.To="001_init", the
	// per-datasource Migrator.resolveName must reject the ambiguity
	// with the candidate list instead of silently picking one.
	reg := migration.NewRegistry()
	_ = reg.AddSource(NewSQLSource("iam", Datasource{Name: "default"}, nil,
		Definition{Name: "001_init", SQL: "SELECT 1"},
	))
	_ = reg.AddSource(NewSQLSource("secrets", Datasource{Name: "default"}, nil,
		Definition{Name: "001_init", SQL: "SELECT 1"},
	))

	runner := newSQLRunner(SQLRunnerOptions{
		Registry:  reg,
		OpenDB:    stubOpenDB(t),
		AutoApply: true,
	})

	_, err := runner.Apply(context.Background(), migration.ApplyOpts{Force: true, To: "001_init"})
	if err == nil {
		t.Fatal("expected ambiguity error for cross-namespace basename collision")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("error must mention ambiguity, got %v", err)
	}
	if !strings.Contains(err.Error(), "iam/001_init") || !strings.Contains(err.Error(), "secrets/001_init") {
		t.Fatalf("error must list both candidate full names, got %v", err)
	}
}

// --- Schema → search_path threading -----------------------------------------

func TestSQLRunner_ThreadsDeclaredSchemaPerDatasource(t *testing.T) {
	// One migration set (same namespace + same migration names) registered
	// against several datasources, each owning its own schema — the
	// "one migration set → many schemas" shape. Each per-datasource Migrator
	// must carry its datasource's schema as the search_path.
	reg := migration.NewRegistry()
	_ = reg.AddSource(NewSQLSource("registry", Datasource{Name: "registry_put", Schema: "registry_put"}, nil,
		Definition{Name: "001_init", SQL: "CREATE TABLE blobs (id int)"}))
	_ = reg.AddSource(NewSQLSource("registry", Datasource{Name: "registry_oci", Schema: "registry_oci"}, nil,
		Definition{Name: "001_init", SQL: "CREATE TABLE blobs (id int)"}))

	runner := newSQLRunner(SQLRunnerOptions{Registry: reg, OpenDB: stubOpenDB(t), AutoApply: true})
	migrators, err := runner.materializeMigrators()
	if err != nil {
		t.Fatalf("materializeMigrators: %v", err)
	}
	if got := migrators["registry_put"].schema; got != "registry_put" {
		t.Errorf("registry_put Migrator schema = %q, want registry_put", got)
	}
	if got := migrators["registry_oci"].schema; got != "registry_oci" {
		t.Errorf("registry_oci Migrator schema = %q, want registry_oci", got)
	}
}

func TestSQLRunner_ConflictingSchemasForDatasource_ReportsInvalidDef(t *testing.T) {
	// Two sources mapping the SAME datasource to DIFFERENT schemas is a
	// contradiction the runner must reject rather than silently pick one.
	reg := migration.NewRegistry()
	_ = reg.AddSource(NewSQLSource("a", Datasource{Name: "shared", Schema: "schema_a"}, nil,
		Definition{Name: "001", SQL: "X"}))
	_ = reg.AddSource(NewSQLSource("b", Datasource{Name: "shared", Schema: "schema_b"}, nil,
		Definition{Name: "001", SQL: "Y"}))

	runner := newSQLRunner(SQLRunnerOptions{Registry: reg, OpenDB: stubOpenDB(t), AutoApply: true})
	_, err := runner.materializeMigrators()
	if err == nil {
		t.Fatal("expected a conflicting-schema error")
	}
	var pe *perrors.Error
	if !stderrors.As(err, &pe) || pe.Code() != CodeMigrationInvalidDef {
		t.Fatalf("expected CodeMigrationInvalidDef, got %v", err)
	}
	if !strings.Contains(err.Error(), `datasource "shared" has conflicting schemas across sources: "schema_a" and "schema_b" (namespaces "a" and "b")`) {
		t.Fatalf("error should name the datasource, both schemas and both sources, got %v", err)
	}
}

func TestSQLRunner_Status_MissingStateStoreReportsDefinitionsPending(t *testing.T) {
	spectest.Proves(t, "go/sql-migration-execution", "read-only-observation", "a-missing-state-store-is-reported-as-all-definitions-pending")

	reg := migration.NewRegistry()
	_ = reg.AddSource(NewSQLSource("iam", Datasource{Name: "primary"}, nil,
		Definition{Name: "001_init", SQL: "CREATE TABLE users (id bigint)"},
	))
	db, store := newFakeMigratorDB(t)
	store.failQueries(&pgconn.PgError{Code: sqlStateUndefinedTable})
	runner := newSQLRunner(SQLRunnerOptions{
		Registry: reg,
		OpenDB:   func() (*stdsql.DB, error) { return db, nil },
	})

	records, err := runner.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("Status returned %d records, want one pending definition", len(records))
	}
	if records[0].Name != "iam/001_init" || records[0].Status != migration.StatusPending {
		t.Fatalf("Status record = %+v, want iam/001_init pending", records[0])
	}
	if got := store.statements(); len(got) != 0 {
		t.Fatalf("Status executed mutating statements %v, want none", got)
	}
}

// --- Test doubles -----------------------------------------------------------

type fakeNonSQLSource struct{ ns string }

func (f fakeNonSQLSource) Kind() migration.Kind { return migration.KindSQL }
func (f fakeNonSQLSource) Namespace() string    { return f.ns }

// TestSQLRunner_AppliesDatasourcesInLexicographicOrder pins the runner half of
// deterministic ordering: with several datasources contributing migrations,
// apply visits them in lexicographic datasource order, not map order. All
// migrators share one fake DB, so the store's body log records the global
// execution order across datasources.
func TestSQLRunner_AppliesDatasourcesInLexicographicOrder(t *testing.T) {
	spectest.Proves(t, "go/sql-migration-execution", "deterministic-execution", "datasources-execute-in-lexicographic-order")
	store := &fakeStore{}
	db := stdsql.OpenDB(&fakeConnector{store: store})
	t.Cleanup(func() { _ = db.Close() })

	reg := migration.NewRegistry()
	for _, ds := range []string{"delta", "alpha", "charlie", "bravo"} {
		_ = reg.AddSource(NewSQLSource("iam", Datasource{Name: ds}, fstest.MapFS{
			"001_init.up.sql": &fstest.MapFile{Data: []byte("BODY-" + ds)},
		}))
	}
	runner := newSQLRunner(SQLRunnerOptions{
		Registry:  reg,
		OpenDB:    func() (*stdsql.DB, error) { return db, nil },
		AutoApply: true,
	})
	if _, err := runner.Apply(context.Background(), migration.ApplyOpts{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	want := []string{"BODY-alpha", "BODY-bravo", "BODY-charlie", "BODY-delta"}
	got := store.bodyLog()
	if len(got) != len(want) {
		t.Fatalf("bodies = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("datasource apply order = %v, want lexicographic %v", got, want)
		}
	}
}
