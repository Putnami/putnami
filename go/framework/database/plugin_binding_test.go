package database

import (
	"reflect"
	"strings"
	"testing"

	"go.putnami.dev/app"
	"go.putnami.dev/protocol/infra"
)

// TestEffectivePoolConfig_ResolvesDatasourceFromEnv proves a named datasource
// takes its connection and search_path from the injected binding while keeping
// the pool's tuning knobs.
func TestEffectivePoolConfig_ResolvesDatasourceFromEnv(t *testing.T) {
	t.Setenv(EnvBinding, `{
		"protocolVersion": 1,
		"databases": { "auth": { "engine": "postgres", "schema": "iam",
			"connection": { "host": "db", "port": 5432, "database": "auth_db", "user": "u", "password": "p", "ssl": false } } }
	}`)
	p := NewPlugin(PluginConfig{
		Datasource: "auth",
		Pool: PoolConfig{
			DSN:        "postgres://local/local",
			SearchPath: "local_schema",
			MaxConns:   7,
		},
	})

	cfg, err := p.effectivePoolConfig()
	if err != nil {
		t.Fatalf("effectivePoolConfig: %v", err)
	}
	if cfg.SearchPath != "iam" {
		t.Errorf("SearchPath = %q, want iam", cfg.SearchPath)
	}
	if cfg.MaxConns != 7 {
		t.Errorf("MaxConns = %d, want the pool's 7 to be preserved", cfg.MaxConns)
	}
	if cfg.DSN == "postgres://local/local" {
		t.Error("binding should override the local fallback DSN when DATABASE_BINDINGS is injected")
	}
	dsn, err := cfg.resolveDSN()
	if err != nil {
		t.Fatalf("resolveDSN: %v", err)
	}
	if !strings.Contains(dsn, "dbname=auth_db") {
		t.Errorf("dsn = %q, want the physical database auth_db", dsn)
	}
}

// TestEffectivePoolConfig_NamedDatasourceFallsBackToLocalPool proves a named
// datasource can run locally from its Pool DSN/SearchPath when no deploy binding
// is injected.
func TestEffectivePoolConfig_NamedDatasourceFallsBackToLocalPool(t *testing.T) {
	t.Setenv(EnvBinding, "")
	p := NewPlugin(PluginConfig{
		Datasource: "auth",
		Pool: PoolConfig{
			DSN:        "postgres://local/auth",
			SearchPath: "iam",
			MaxConns:   7,
		},
	})

	cfg, err := p.effectivePoolConfig()
	if err != nil {
		t.Fatalf("effectivePoolConfig: %v", err)
	}
	if cfg.DSN != "postgres://local/auth" {
		t.Errorf("DSN = %q, want local fallback DSN", cfg.DSN)
	}
	if cfg.SearchPath != "iam" {
		t.Errorf("SearchPath = %q, want local fallback search_path", cfg.SearchPath)
	}
	if cfg.MaxConns != 7 {
		t.Errorf("MaxConns = %d, want tuning preserved", cfg.MaxConns)
	}
}

// TestEffectivePoolConfig_NamedDatasourceStructuredConnectionFallsBackToLocalPool
// proves a structured local Connection can use the plugin's logical datasource
// name as the database target when no deploy binding is injected.
func TestEffectivePoolConfig_NamedDatasourceStructuredConnectionFallsBackToLocalPool(t *testing.T) {
	t.Setenv(EnvBinding, "")
	p := NewPlugin(PluginConfig{
		Datasource: "auth",
		Pool: PoolConfig{
			Connection: Connection{Host: "localhost", Port: 5432, User: "u", Password: "p"},
			SearchPath: "iam",
			MaxConns:   7,
		},
	})

	cfg, err := p.effectivePoolConfig()
	if err != nil {
		t.Fatalf("effectivePoolConfig: %v", err)
	}
	if cfg.Database != "auth" {
		t.Errorf("Database = %q, want the datasource name auth", cfg.Database)
	}
	dsn, err := cfg.resolveDSN()
	if err != nil {
		t.Fatalf("resolveDSN: %v", err)
	}
	if !strings.Contains(dsn, "dbname=auth") {
		t.Errorf("dsn = %q, want the datasource name as the database target", dsn)
	}
	if cfg.SearchPath != "iam" {
		t.Errorf("SearchPath = %q, want local fallback search_path", cfg.SearchPath)
	}
}

// TestEffectivePoolConfig_MissingBindingAndLocalConnectionErrors fails loudly
// when a datasource is named but neither a deploy binding nor local connection
// fallback is configured.
func TestEffectivePoolConfig_MissingBindingAndLocalConnectionErrors(t *testing.T) {
	t.Setenv(EnvBinding, "")
	p := NewPlugin(PluginConfig{Datasource: "auth"})
	_, err := p.effectivePoolConfig()
	if err == nil {
		t.Fatal("expected an error when a datasource is named but no binding or local connection is configured")
	}
	if !strings.Contains(err.Error(), EnvBinding) {
		t.Errorf("error %q should mention %s", err.Error(), EnvBinding)
	}
	if !strings.Contains(err.Error(), "Pool") {
		t.Errorf("error %q should mention the local Pool fallback", err.Error())
	}
}

// TestEffectivePoolConfig_InjectedBindingErrorsEvenWithLocalPool proves an
// injected binding is authoritative: malformed deploy configuration fails
// loudly instead of silently falling back to a local DSN.
func TestEffectivePoolConfig_InjectedBindingErrorsEvenWithLocalPool(t *testing.T) {
	t.Setenv(EnvBinding, `{not-json`)
	p := NewPlugin(PluginConfig{Datasource: "auth", Pool: PoolConfig{DSN: "postgres://local/auth"}})

	_, err := p.effectivePoolConfig()
	if err == nil {
		t.Fatal("expected an invalid injected binding to fail")
	}
	if !strings.Contains(err.Error(), "invalid "+EnvBinding) {
		t.Errorf("error %q should describe the invalid injected binding", err.Error())
	}
}

// TestEffectivePoolConfig_NoDatasourceUsesPool keeps the explicit Pool
// connection (escape hatch) when no datasource is named.
func TestEffectivePoolConfig_NoDatasourceUsesPool(t *testing.T) {
	p := NewPlugin(PluginConfig{Pool: PoolConfig{DSN: "postgres://x/y"}})
	cfg, err := p.effectivePoolConfig()
	if err != nil {
		t.Fatalf("effectivePoolConfig: %v", err)
	}
	if cfg.DSN != "postgres://x/y" {
		t.Errorf("DSN = %q, want the explicit pool DSN", cfg.DSN)
	}
}

// TestPluginDescribe_DatasourceDerivesInfraViaBridge proves the build emits the
// infra requirement for the named datasource through the database-protocol
// projection bridge: a single {name, engine} entry, with the schema left to the
// runtime binding and migration sources.
func TestPluginDescribe_DatasourceDerivesInfraViaBridge(t *testing.T) {
	out := t.TempDir()
	p := NewPlugin(PluginConfig{Datasource: "auth"})

	if err := p.Describe(&app.DescribeContext{OutputDir: out, Targets: []string{"all"}}); err != nil {
		t.Fatalf("describe: %v", err)
	}

	manifest, diags := infra.LoadPerProjectManifest(infra.SidecarPathIn(out, "database"))
	if d := firstError(diags); d != "" {
		t.Fatalf("loaded sidecar has errors: %s", d)
	}
	if manifest == nil || len(manifest.Databases) != 1 {
		t.Fatalf("expected 1 database, got %+v", manifest)
	}
	got := manifest.Databases[0]
	want := infra.Database{Name: "auth", Engine: infra.EnginePostgres}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("database = %+v, want %+v (no schema at build time)", got, want)
	}
}

// TestPluginDescribe_InvalidDatasourceNameErrors turns an unusable datasource
// name into a build-time error via the infra validation the bridge feeds.
func TestPluginDescribe_InvalidDatasourceNameErrors(t *testing.T) {
	out := t.TempDir()
	p := NewPlugin(PluginConfig{Datasource: "Not A Name"})
	if err := p.Describe(&app.DescribeContext{OutputDir: out, Targets: []string{"all"}}); err == nil {
		t.Fatal("expected an error for an invalid datasource name")
	}
}
