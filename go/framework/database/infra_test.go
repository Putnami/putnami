package database

import (
	"os"
	"reflect"
	"testing"

	"go.putnami.dev/app"
	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/infra"
)

func TestBuildInfraManifest_ZeroPools(t *testing.T) {
	m, diags := buildInfraManifest(infra.EnginePostgres, nil)
	if len(diags) != 0 {
		t.Fatalf("expected no diagnostics, got %v", diags)
	}
	if m != nil {
		t.Fatalf("expected nil manifest when nothing is declared, got %+v", m)
	}
}

func TestBuildInfraManifest_SinglePool(t *testing.T) {
	m, diags := buildInfraManifest(infra.EnginePostgres, []databaseDecl{
		{name: "primary", schemas: []string{"iam"}},
	})
	if len(diags) != 0 {
		t.Fatalf("expected no diagnostics, got %v", diags)
	}
	if m == nil {
		t.Fatal("expected a manifest")
	}
	if m.ProtocolVersion != infra.ProtocolVersion {
		t.Errorf("protocolVersion = %d, want %d", m.ProtocolVersion, infra.ProtocolVersion)
	}
	if len(m.Databases) != 1 {
		t.Fatalf("expected 1 database, got %d", len(m.Databases))
	}
	got := m.Databases[0]
	want := infra.Database{Name: "primary", Engine: infra.EnginePostgres, Schemas: []string{"iam"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("database = %+v, want %+v", got, want)
	}
}

func TestBuildInfraManifest_MultiPool(t *testing.T) {
	m, diags := buildInfraManifest(infra.EnginePostgres, []databaseDecl{
		{name: "primary", schemas: []string{"iam"}},
		{name: "cache"},
	})
	if len(diags) != 0 {
		t.Fatalf("expected no diagnostics, got %v", diags)
	}
	if len(m.Databases) != 2 {
		t.Fatalf("expected 2 databases, got %d", len(m.Databases))
	}
	// Databases are emitted sorted by name for deterministic output.
	if m.Databases[0].Name != "cache" || m.Databases[1].Name != "primary" {
		t.Errorf("databases not sorted by name: %+v", m.Databases)
	}
	if len(m.Databases[0].Schemas) != 0 {
		t.Errorf("cache should have no schemas, got %v", m.Databases[0].Schemas)
	}
}

func TestBuildInfraManifest_MultiSchema(t *testing.T) {
	// Two decls for the same database with different schemas merge into a
	// sorted, de-duplicated union.
	m, diags := buildInfraManifest(infra.EnginePostgres, []databaseDecl{
		{name: "primary", schemas: []string{"iam", "audit"}},
		{name: "primary", schemas: []string{"iam", "billing"}},
	})
	if len(diags) != 0 {
		t.Fatalf("expected no diagnostics, got %v", diags)
	}
	if len(m.Databases) != 1 {
		t.Fatalf("expected 1 merged database, got %d", len(m.Databases))
	}
	want := []string{"audit", "billing", "iam"}
	if !reflect.DeepEqual(m.Databases[0].Schemas, want) {
		t.Errorf("schemas = %v, want %v", m.Databases[0].Schemas, want)
	}
}

func TestBuildInfraManifest_InvalidEngine(t *testing.T) {
	_, diags := buildInfraManifest(infra.Engine("oracle"), []databaseDecl{
		{name: "primary"},
	})
	if len(diags) == 0 {
		t.Fatal("expected a diagnostic for an invalid engine")
	}
	if diags[0].Code != infra.ErrorCodeInvalidEngine {
		t.Errorf("diagnostic code = %q, want %q", diags[0].Code, infra.ErrorCodeInvalidEngine)
	}
}

func TestDeriveDatabaseDecls_PoolSchemas(t *testing.T) {
	// The pool contributes its own database and the schemas it references
	// directly. Schemas owned by SQL migration sources are emitted by the
	// migration registry (via SQLSource.InfraDatabases), not derived here, so
	// this path no longer guesses a schema from a migration namespace.
	pool := PoolConfig{Database: "primary", Schemas: []string{"app"}}

	decls := deriveDatabaseDecls(pool)

	m, diags := buildInfraManifest(infra.EnginePostgres, decls)
	if len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if len(m.Databases) != 1 {
		t.Fatalf("expected 1 database, got %d", len(m.Databases))
	}
	if m.Databases[0].Name != "primary" {
		t.Errorf("database name = %q, want primary", m.Databases[0].Name)
	}
	if !reflect.DeepEqual(m.Databases[0].Schemas, []string{"app"}) {
		t.Errorf("primary schemas = %v, want [app]", m.Databases[0].Schemas)
	}
}

func TestDeriveDatabaseDecls_DefaultDatasource(t *testing.T) {
	decls := deriveDatabaseDecls(PoolConfig{})
	if len(decls) != 1 {
		t.Fatalf("expected 1 decl, got %d", len(decls))
	}
	if decls[0].name != "default" {
		t.Errorf("default pool database = %q, want %q", decls[0].name, "default")
	}
}

func TestDeriveDatabaseDecls_DeclaredDatasource(t *testing.T) {
	// A declared datasource is authoritative even when only a raw DSN supplies
	// the connection: the emitted requirement names the declared (name, schema)
	// rather than defaulting to "default" with no schema. Pool.Schemas merge in.
	pool := PoolConfig{
		DSN:        "postgres:///platform?host=/cloudsql/p:r:i",
		Datasource: Datasource{Name: "platform", Schema: "public"},
		Schemas:    []string{"audit"},
	}

	decls := deriveDatabaseDecls(pool)
	if len(decls) != 1 {
		t.Fatalf("expected 1 decl, got %d", len(decls))
	}
	if decls[0].name != "platform" {
		t.Errorf("declared datasource name = %q, want platform", decls[0].name)
	}

	m, diags := buildInfraManifest(infra.EnginePostgres, decls)
	if len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if got, want := m.Databases[0].Schemas, []string{"audit", "public"}; !reflect.DeepEqual(got, want) {
		t.Errorf("schemas = %v, want %v", got, want)
	}
}

func TestPluginDescribe_DeclaredDatasource(t *testing.T) {
	out := t.TempDir()
	p := NewPlugin(PluginConfig{Pool: PoolConfig{
		DSN:        "postgres:///platform?host=/cloudsql/p:r:i",
		Datasource: Datasource{Name: "platform", Schema: "public"},
	}})

	if err := p.Describe(&app.DescribeContext{OutputDir: out, Targets: []string{"all"}}); err != nil {
		t.Fatalf("describe: %v", err)
	}

	manifest, diags := infra.LoadPerProjectManifest(infra.SidecarPathIn(out, "database"))
	if diag := firstError(diags); diag != "" {
		t.Fatalf("loaded sidecar has errors: %s", diag)
	}
	if manifest == nil || len(manifest.Databases) != 1 {
		t.Fatalf("expected 1 database, got %+v", manifest)
	}
	db := manifest.Databases[0]
	if db.Name != "platform" || !reflect.DeepEqual(db.Schemas, []string{"public"}) {
		t.Errorf("database = %+v, want platform/[public]", db)
	}
}

func TestPluginDescribe_PartialDatasourceErrors(t *testing.T) {
	out := t.TempDir()
	// Name without schema: an incomplete declaration must fail the build.
	p := NewPlugin(PluginConfig{Pool: PoolConfig{Datasource: Datasource{Name: "platform"}}})
	if err := p.Describe(&app.DescribeContext{OutputDir: out, Targets: []string{"all"}}); err == nil {
		t.Fatal("expected an error for a partial datasource declaration")
	}
}

func firstError(diags []diag.Diagnostic) string {
	for _, d := range diags {
		if d.Severity == "error" {
			return d.Message
		}
	}
	return ""
}

func TestPluginDescribe_WritesSidecar(t *testing.T) {
	out := t.TempDir()
	p := NewPlugin(PluginConfig{Pool: PoolConfig{Database: "primary", Schemas: []string{"app"}}})

	if err := p.Describe(&app.DescribeContext{OutputDir: out, Targets: []string{"all"}}); err != nil {
		t.Fatalf("describe: %v", err)
	}

	path := infra.SidecarPathIn(out, "database")
	manifest, diags := infra.LoadPerProjectManifest(path)
	for _, d := range diags {
		if d.Severity == "error" {
			t.Fatalf("loaded sidecar has errors: %v", diags)
		}
	}
	if manifest == nil {
		t.Fatal("expected a parsed manifest")
	}
	if len(manifest.Databases) != 1 {
		t.Fatalf("expected 1 database, got %d", len(manifest.Databases))
	}
	db := manifest.Databases[0]
	if db.Name != "primary" || db.Engine != infra.EnginePostgres {
		t.Errorf("database = %+v, want primary/postgres", db)
	}
	// The database producer declares only the pool's own schemas now; schemas
	// owned by SQL migration sources land in the migration fragment instead.
	if !reflect.DeepEqual(db.Schemas, []string{"app"}) {
		t.Errorf("schemas = %v, want [app]", db.Schemas)
	}
}

func TestPluginDescribe_SkippedWhenNotWanted(t *testing.T) {
	out := t.TempDir()
	p := NewPlugin(PluginConfig{})
	if err := p.Describe(&app.DescribeContext{OutputDir: out, Targets: []string{"openapi"}}); err != nil {
		t.Fatalf("describe: %v", err)
	}
	path := infra.SidecarPathIn(out, "database")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected no sidecar when describer is not targeted, stat err = %v", err)
	}
}

func TestPluginDescribe_InvalidEngineErrors(t *testing.T) {
	out := t.TempDir()
	p := NewPlugin(PluginConfig{Pool: PoolConfig{Engine: infra.Engine("oracle")}})
	err := p.Describe(&app.DescribeContext{OutputDir: out, Targets: []string{"all"}})
	if err == nil {
		t.Fatal("expected an error for an invalid engine")
	}
}
