package testprovider

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.putnami.dev/database"
	pdb "go.putnami.dev/protocol/database"
)

func tcp(db string) *pdb.Connection {
	return &pdb.Connection{Host: "h", Port: 5432, Database: db, User: "postgres", Password: "postgres"}
}

// fixedSuffix returns a deterministic suffix generator for plan assertions.
func fixedSuffix(values ...string) func() string {
	i := 0
	return func() string {
		v := values[i%len(values)]
		i++
		return v
	}
}

func TestScopeSources_FiltersByDatasourceAndStripsSchema(t *testing.T) {
	// The provider owns the search_path through its isolated pool, so scoped
	// sources must be schema-neutral — otherwise the runner would override the
	// isolated/owning schema with the source's bundle-declared one.
	sources := []database.SQLSource{
		database.NewSQLSource("registry", database.Datasource{Name: "registry_put", Schema: "registry_put"}, nil,
			database.Definition{Name: "001", SQL: "X"}),
		database.NewSQLSource("registry", database.Datasource{Name: "registry_oci", Schema: "registry_oci"}, nil,
			database.Definition{Name: "001", SQL: "Y"}),
	}

	scoped := scopeSources(sources, "registry_put")
	if len(scoped) != 1 {
		t.Fatalf("scopeSources kept %d sources, want only registry_put", len(scoped))
	}
	if scoped[0].Datasource() != "registry_put" {
		t.Errorf("scoped datasource = %q, want registry_put", scoped[0].Datasource())
	}
	if scoped[0].Schema() != "" {
		t.Errorf("scoped source schema = %q, want it neutralized to empty", scoped[0].Schema())
	}
}

func TestResolveTestBinding(t *testing.T) {
	t.Run("blank yields nil", func(t *testing.T) {
		tb, err := ResolveTestBinding("   ")
		if err != nil || tb != nil {
			t.Fatalf("blank binding: tb=%v err=%v, want nil,nil", tb, err)
		}
	})
	t.Run("valid parses", func(t *testing.T) {
		tb, err := ResolveTestBinding(`{
			"protocolVersion": 1, "mode": "require", "isolation": "database", "applyMigrations": true,
			"databases": { "auth": { "engine": "postgres", "schema": "iam",
				"connection": { "host": "localhost", "port": 5432, "database": "auth", "user": "postgres", "password": "postgres", "ssl": false } } }
		}`)
		if err != nil {
			t.Fatalf("ResolveTestBinding: %v", err)
		}
		if tb.Mode != pdb.TestModeRequire || tb.Isolation != pdb.IsolationDatabase || !tb.ApplyMigrations {
			t.Errorf("policy = %+v, want require/database/applyMigrations", tb)
		}
	})
	t.Run("invalid rejected", func(t *testing.T) {
		if _, err := ResolveTestBinding(`{"protocolVersion": 1, "databases": { "auth": { "engine": "mysql" } }}`); err == nil {
			t.Fatal("expected an error for an invalid test binding")
		}
	})
}

func TestEffectiveModeDefaultsToRequire(t *testing.T) {
	if got := effectiveMode(nil); got != pdb.TestModeRequire {
		t.Errorf("effectiveMode(nil) = %q, want require", got)
	}
	if got := effectiveMode(&pdb.TestBinding{}); got != pdb.TestModeRequire {
		t.Errorf("effectiveMode(empty) = %q, want require", got)
	}
	if got := effectiveMode(&pdb.TestBinding{Mode: pdb.TestModeSkip}); got != pdb.TestModeSkip {
		t.Errorf("effectiveMode(skip) = %q, want skip", got)
	}
}

func TestPlanDatabases_DatabaseIsolation(t *testing.T) {
	tb := &pdb.TestBinding{
		Isolation: pdb.IsolationDatabase,
		Databases: map[string]pdb.Database{
			"billing": {Engine: pdb.EnginePostgres, Schema: "billing", Connection: tcp("billing")},
			"auth":    {Engine: pdb.EnginePostgres, Schema: "iam", Connection: tcp("auth")},
		},
	}
	plans, err := planDatabases(tb, fixedSuffix("aaa", "bbb"))
	if err != nil {
		t.Fatalf("planDatabases: %v", err)
	}
	if len(plans) != 2 || plans[0].name != "auth" || plans[1].name != "billing" {
		t.Fatalf("plans not sorted by name: %+v", plans)
	}
	if plans[0].isoDB != "auth_t_aaa" {
		t.Errorf("auth isoDB = %q, want auth_t_aaa", plans[0].isoDB)
	}
	if plans[1].isoDB != "billing_t_bbb" {
		t.Errorf("billing isoDB = %q, want billing_t_bbb", plans[1].isoDB)
	}
	if plans[0].isoSchema != "iam" {
		t.Errorf("auth isoSchema = %q, want the owning schema iam", plans[0].isoSchema)
	}
}

func TestPlanDatabases_ThreadsKeepDatabases(t *testing.T) {
	tb := &pdb.TestBinding{
		Isolation:     pdb.IsolationDatabase,
		KeepDatabases: true,
		Databases: map[string]pdb.Database{
			"auth": {Engine: pdb.EnginePostgres, Schema: "iam", Connection: tcp("auth")},
		},
	}
	plans, err := planDatabases(tb, fixedSuffix("aaa"))
	if err != nil {
		t.Fatalf("planDatabases: %v", err)
	}
	if !plans[0].keep {
		t.Error("keepDatabases=true not threaded into the plan; teardown would still DROP")
	}
	tb.KeepDatabases = false
	plans, err = planDatabases(tb, fixedSuffix("aaa"))
	if err != nil {
		t.Fatalf("planDatabases: %v", err)
	}
	if plans[0].keep {
		t.Error("keep set without keepDatabases; per-suite databases would leak on a retained server")
	}
}

func TestPlanDatabases_SchemaIsolation(t *testing.T) {
	tb := &pdb.TestBinding{
		Isolation: pdb.IsolationSchema,
		Databases: map[string]pdb.Database{
			"auth": {Engine: pdb.EnginePostgres, Schema: "iam", Connection: tcp("auth")},
		},
	}
	plans, err := planDatabases(tb, fixedSuffix("zzz"))
	if err != nil {
		t.Fatalf("planDatabases: %v", err)
	}
	if plans[0].isoSchema != "iam_t_zzz" {
		t.Errorf("isoSchema = %q, want iam_t_zzz", plans[0].isoSchema)
	}
	if plans[0].isoDB != "" {
		t.Errorf("schema isolation should not allocate an isolated database, got %q", plans[0].isoDB)
	}
}

func TestPlanDatabases_DefaultsToDatabaseIsolation(t *testing.T) {
	tb := &pdb.TestBinding{
		Databases: map[string]pdb.Database{"auth": {Engine: pdb.EnginePostgres, Schema: "iam", Connection: tcp("auth")}},
	}
	plans, err := planDatabases(tb, fixedSuffix("s"))
	if err != nil {
		t.Fatalf("planDatabases: %v", err)
	}
	if plans[0].isolation != pdb.IsolationDatabase || plans[0].isoDB == "" {
		t.Errorf("expected default database isolation, got %+v", plans[0])
	}
}

func TestPlanDatabases_Errors(t *testing.T) {
	t.Run("unsupported engine", func(t *testing.T) {
		tb := &pdb.TestBinding{Databases: map[string]pdb.Database{"a": {Engine: pdb.Engine("mysql"), Connection: tcp("a")}}}
		if _, err := planDatabases(tb, fixedSuffix("s")); err == nil {
			t.Fatal("expected an error for an unsupported engine")
		}
	})
	t.Run("missing connection", func(t *testing.T) {
		tb := &pdb.TestBinding{Databases: map[string]pdb.Database{"a": {Engine: pdb.EnginePostgres, Schema: "s"}}}
		if _, err := planDatabases(tb, fixedSuffix("s")); err == nil {
			t.Fatal("expected an error for a missing connection")
		}
	})
}

func TestRuntimeBinding_DatabaseIsolationSwapsDatabase(t *testing.T) {
	plans := []plan{{
		name: "auth", isolation: pdb.IsolationDatabase,
		entry: pdb.Database{Engine: pdb.EnginePostgres, Schema: "iam", Connection: tcp("auth")},
		isoDB: "auth_t_x", isoSchema: "iam",
	}}
	b, err := runtimeBinding(plans)
	if err != nil {
		t.Fatalf("runtimeBinding: %v", err)
	}
	got := b.Databases["auth"]
	if got.Connection.Database != "auth_t_x" {
		t.Errorf("connection database = %q, want the isolated auth_t_x", got.Connection.Database)
	}
	if got.Schema != "iam" {
		t.Errorf("schema = %q, want the owning schema iam preserved as search_path", got.Schema)
	}
	if b.ProtocolVersion != pdb.ProtocolVersion {
		t.Errorf("protocolVersion = %d, want %d", b.ProtocolVersion, pdb.ProtocolVersion)
	}
}

func TestRuntimeBinding_SchemaIsolationUsesIsolatedSchema(t *testing.T) {
	plans := []plan{{
		name: "auth", isolation: pdb.IsolationSchema,
		entry:     pdb.Database{Engine: pdb.EnginePostgres, Schema: "iam", Connection: tcp("auth")},
		isoSchema: "iam_t_x",
	}}
	b, err := runtimeBinding(plans)
	if err != nil {
		t.Fatalf("runtimeBinding: %v", err)
	}
	got := b.Databases["auth"]
	if got.Schema != "iam_t_x" {
		t.Errorf("schema = %q, want the isolated schema as search_path", got.Schema)
	}
	if got.Connection.Database != "auth" {
		t.Errorf("connection database = %q, want the base auth unchanged", got.Connection.Database)
	}
}

func TestRuntimeBinding_DoesNotMutateInput(t *testing.T) {
	conn := tcp("auth")
	plans := []plan{{
		name: "auth", isolation: pdb.IsolationDatabase,
		entry: pdb.Database{Engine: pdb.EnginePostgres, Schema: "iam", Connection: conn},
		isoDB: "auth_t_x", isoSchema: "iam",
	}}
	if _, err := runtimeBinding(plans); err != nil {
		t.Fatalf("runtimeBinding: %v", err)
	}
	if conn.Database != "auth" {
		t.Errorf("input connection was mutated: database = %q, want auth", conn.Database)
	}
}

func TestRuntimeBinding_DSNDatabaseRewrite(t *testing.T) {
	plans := []plan{{
		name: "events", isolation: pdb.IsolationDatabase,
		entry: pdb.Database{Engine: pdb.EnginePostgres, Schema: "events", Connection: &pdb.Connection{DSN: "postgres://u:p@h:5432/events?sslmode=disable"}},
		isoDB: "events_t_x", isoSchema: "events",
	}}
	b, err := runtimeBinding(plans)
	if err != nil {
		t.Fatalf("runtimeBinding: %v", err)
	}
	dsn := b.Databases["events"].Connection.DSN
	if !strings.Contains(dsn, "/events_t_x") {
		t.Errorf("dsn = %q, want the database rewritten to events_t_x", dsn)
	}
	if !strings.Contains(dsn, "sslmode=disable") {
		t.Errorf("dsn = %q, want the sslmode query preserved", dsn)
	}
}

func TestConnectionDatabase(t *testing.T) {
	if got, _ := connectionDatabase(tcp("auth")); got != "auth" {
		t.Errorf("structured database = %q, want auth", got)
	}
	got, err := connectionDatabase(&pdb.Connection{DSN: "postgres://u:p@h:5432/events"})
	if err != nil {
		t.Fatalf("connectionDatabase(dsn): %v", err)
	}
	if got != "events" {
		t.Errorf("dsn database = %q, want events", got)
	}
}

func TestPgIdent(t *testing.T) {
	if got := pgIdent("Auth-DB", "t", "abc123"); got != "auth_db_t_abc123" {
		t.Errorf("pgIdent = %q, want auth_db_t_abc123 (lowercased, sanitized)", got)
	}
	long := pgIdent(strings.Repeat("x", 100), "t", "abcdef")
	if len(long) > 63 {
		t.Errorf("pgIdent length = %d, want <= 63", len(long))
	}
	if !strings.HasSuffix(long, "_t_abcdef") {
		t.Errorf("pgIdent = %q, want the suffix preserved after clamping", long)
	}
}

func TestIsolationOfDefaultsToDatabase(t *testing.T) {
	if got := isolationOf(&pdb.TestBinding{}); got != pdb.IsolationDatabase {
		t.Errorf("isolationOf(empty) = %q, want database", got)
	}
	if got := isolationOf(&pdb.TestBinding{Isolation: pdb.IsolationSchema}); got != pdb.IsolationSchema {
		t.Errorf("isolationOf(schema) = %q, want schema", got)
	}
}

func TestReuseTemplates(t *testing.T) {
	cases := []struct {
		name  string
		tb    *pdb.TestBinding
		apply bool
		want  bool
	}{
		{"reuse+apply+database", &pdb.TestBinding{Reuse: pdb.ReuseBundleTemplate, Isolation: pdb.IsolationDatabase}, true, true},
		{"reuse default isolation", &pdb.TestBinding{Reuse: pdb.ReuseBundleTemplate}, true, true},
		{"reuse but no apply", &pdb.TestBinding{Reuse: pdb.ReuseBundleTemplate}, false, false},
		{"reuse none", &pdb.TestBinding{Reuse: pdb.ReuseNone}, true, false},
		{"reuse with schema isolation", &pdb.TestBinding{Reuse: pdb.ReuseBundleTemplate, Isolation: pdb.IsolationSchema}, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := reuseTemplates(c.tb, c.apply); got != c.want {
				t.Errorf("reuseTemplates = %v, want %v", got, c.want)
			}
		})
	}
}

func TestTemplateName(t *testing.T) {
	got := templateName("auth", "abcdef0123456789")
	if got != "auth_tmpl_abcdef012345" {
		t.Errorf("templateName = %q, want auth_tmpl_abcdef012345 (12-char digest)", got)
	}
	// A different digest yields a different template; the same digest is stable.
	if templateName("auth", "abcdef0123456789") != got {
		t.Error("templateName should be deterministic for the same base+digest")
	}
	if templateName("auth", "ffffffffffffffff") == got {
		t.Error("templateName should differ for a different digest")
	}
}

func TestScopeSources(t *testing.T) {
	def := database.Definition{Name: "001_init", SQL: "SELECT 1"}
	sources := []database.SQLSource{
		database.NewSQLSource("ns", database.Datasource{Name: "auth", Schema: "iam"}, nil, def),
		database.NewSQLSource("ns", database.Datasource{Name: "billing", Schema: "billing"}, nil, def),
		database.NewSQLSource("ns2", database.Datasource{Name: "auth", Schema: "iam"}, nil, def),
	}
	scoped := scopeSources(sources, "auth")
	if len(scoped) != 2 {
		t.Fatalf("scopeSources(auth) returned %d sources, want 2", len(scoped))
	}
	for _, s := range scoped {
		if s.Datasource() != "auth" {
			t.Errorf("scoped source targets %q, want auth", s.Datasource())
		}
	}
	if len(scopeSources(sources, "ledger")) != 0 {
		t.Error("scopeSources for an unknown datasource should be empty")
	}
}

func TestProvision_NoBindingHonorsMode(t *testing.T) {
	t.Run("skip yields ErrSkip", func(t *testing.T) {
		_, err := Provision(context.Background(), Options{Binding: &pdb.TestBinding{Mode: pdb.TestModeSkip}})
		if !errors.Is(err, ErrSkip) {
			t.Fatalf("err = %v, want ErrSkip", err)
		}
	})
	t.Run("require fails loudly", func(t *testing.T) {
		_, err := Provision(context.Background(), Options{Binding: &pdb.TestBinding{Mode: pdb.TestModeRequire}})
		if err == nil || errors.Is(err, ErrSkip) {
			t.Fatalf("err = %v, want a loud non-skip error", err)
		}
		if !strings.Contains(err.Error(), EnvTestBinding) {
			t.Errorf("error %q should mention %s", err.Error(), EnvTestBinding)
		}
	})
}
