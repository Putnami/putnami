package database

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// configSectionYAML is a CONFIG_DATA document whose "database" section carries
// the managed binding document alongside a non-colliding operator-authored key,
// exactly as the control plane's resolve overlay produces it.
const configSectionYAML = `
database:
  operatorKey: keep-me
  protocolVersion: 1
  databases:
    auth:
      engine: postgres
      schema: iam
      connection:
        host: cfg-db
        port: 5432
        database: auth_cfg
        user: cfg_user
        password: cfg_pass
        ssl: false
`

// envBindingJSON is a DATABASE_BINDINGS document for the same datasource with
// a different physical connection, so tests can tell which transport won.
const envBindingJSON = `{
	"protocolVersion": 1,
	"databases": { "auth": { "engine": "postgres", "schema": "iam_env",
		"connection": { "host": "env-db", "port": 5432, "database": "auth_env", "user": "u", "password": "p", "ssl": false } } }
}`

// TestBindingFromConfigDoc_ParsesTypedSectionValues proves the loose section
// view round-trips YAML-typed values (int port, bool ssl) through the strict
// protocol parse.
func TestBindingFromConfigDoc_ParsesTypedSectionValues(t *testing.T) {
	doc := bindingConfigDoc{
		ProtocolVersion: 1,
		Databases: map[string]any{
			"auth": map[string]any{
				"engine": "postgres",
				"schema": "iam",
				"connection": map[string]any{
					"host": "cfg-db", "port": 5432, "database": "auth_cfg",
					"user": "cfg_user", "password": "cfg_pass", "ssl": false,
				},
			},
		},
	}
	b, err := bindingFromConfigDoc(doc)
	if err != nil {
		t.Fatalf("bindingFromConfigDoc: %v", err)
	}
	if b == nil {
		t.Fatal("expected a parsed binding")
	}
	db, ok := b.Databases["auth"]
	if !ok {
		t.Fatalf("binding lacks datasource auth: %+v", b.Databases)
	}
	if db.Connection == nil || db.Connection.Host != "cfg-db" || db.Connection.Port != 5432 {
		t.Errorf("connection = %+v, want host cfg-db port 5432", db.Connection)
	}
	if db.Connection.SSL == nil || *db.Connection.SSL {
		t.Errorf("ssl = %v, want false", db.Connection.SSL)
	}
}

// TestBindingFromConfigDoc_NoDocumentFallsThrough returns (nil, nil) when the
// section carries no databases key or an empty one, so the env transport and
// local fallbacks stay in charge.
func TestBindingFromConfigDoc_NoDocumentFallsThrough(t *testing.T) {
	for name, doc := range map[string]bindingConfigDoc{
		"absent": {},
		"empty":  {ProtocolVersion: 1, Databases: map[string]any{}},
	} {
		b, err := bindingFromConfigDoc(doc)
		if err != nil {
			t.Errorf("%s: unexpected error: %v", name, err)
		}
		if b != nil {
			t.Errorf("%s: expected no binding, got %+v", name, b)
		}
	}
}

// TestBindingFromConfigDoc_MalformedDocumentErrors proves a section that
// carries a databases key the protocol rejects fails loudly — naming the
// config section — instead of silently degrading to env or local fallbacks.
func TestBindingFromConfigDoc_MalformedDocumentErrors(t *testing.T) {
	for name, doc := range map[string]bindingConfigDoc{
		"non-map databases":       {ProtocolVersion: 1, Databases: "not-a-map"},
		"missing protocolVersion": {Databases: map[string]any{"auth": map[string]any{"engine": "postgres"}}},
	} {
		_, err := bindingFromConfigDoc(doc)
		if err == nil {
			t.Errorf("%s: expected an error", name)
			continue
		}
		if !strings.Contains(err.Error(), configSourceLabel) {
			t.Errorf("%s: error %q should name %s", name, err.Error(), configSourceLabel)
		}
	}
}

// TestHasManagedBinding proves the transport-agnostic presence predicate: true
// when the "database" config section carries a binding document or
// DATABASE_BINDINGS is present, and — crucially — true for a present-but-
// malformed section so a misconfiguration never silently reads as "no managed
// DB" (which would degrade prod to a local/in-memory backend). Absent from both
// transports is the only false, keeping local serve/test on their local path.
func TestHasManagedBinding(t *testing.T) {
	const malformedSection = `
database:
  databases:
    auth:
      engine: mysql
`
	const emptyDatabases = `
database:
  protocolVersion: 1
  databases: {}
`
	for name, tc := range map[string]struct {
		config string
		env    string
		want   bool
	}{
		"config section carries binding": {config: configSectionYAML, want: true},
		"env fallback only":              {env: envBindingJSON, want: true},
		"both transports present":        {config: configSectionYAML, env: envBindingJSON, want: true},
		"malformed section is present":   {config: malformedSection, want: true},
		"empty databases falls to env":   {config: emptyDatabases, env: envBindingJSON, want: true},
		"empty section, no env":          {config: emptyDatabases, want: false},
		"neither transport present":      {want: false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(EnvBinding, tc.env)
			t.Setenv("CONFIG_DATA", tc.config)
			if got := HasManagedBinding(context.Background()); got != tc.want {
				t.Errorf("HasManagedBinding() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestEffectivePoolConfig_ConfigSectionWinsOverEnv proves the managed document
// resolved from the config section is preferred over DATABASE_BINDINGS when
// both are present.
func TestEffectivePoolConfig_ConfigSectionWinsOverEnv(t *testing.T) {
	t.Setenv(EnvBinding, envBindingJSON)
	t.Setenv("CONFIG_DATA", configSectionYAML)
	p := NewPlugin(PluginConfig{Datasource: "auth", Pool: PoolConfig{MaxConns: 7}})
	if err := p.Configure(context.Background(), nil); err != nil {
		t.Fatalf("configure: %v", err)
	}

	cfg, err := p.effectivePoolConfig()
	if err != nil {
		t.Fatalf("effectivePoolConfig: %v", err)
	}
	if cfg.Connection.Host != "cfg-db" || cfg.Database != "auth_cfg" {
		t.Errorf("connection = host %q database %q, want the config-section binding (cfg-db/auth_cfg)", cfg.Connection.Host, cfg.Database)
	}
	if cfg.SearchPath != "iam" {
		t.Errorf("SearchPath = %q, want iam from the config-section binding", cfg.SearchPath)
	}
	if cfg.MaxConns != 7 {
		t.Errorf("MaxConns = %d, want the pool's tuning preserved", cfg.MaxConns)
	}
}

// TestEffectivePoolConfig_ConfigBindingIsAuthoritative proves a config-carried
// document that does not declare the datasource fails loudly instead of
// falling through to an env document that does — config-first is what makes
// the eventual env retirement a verified no-op.
func TestEffectivePoolConfig_ConfigBindingIsAuthoritative(t *testing.T) {
	t.Setenv(EnvBinding, envBindingJSON)
	t.Setenv("CONFIG_DATA", `
database:
  protocolVersion: 1
  databases:
    billing:
      engine: postgres
      schema: billing
      connection:
        dsn: postgres://cfg/billing
`)
	p := NewPlugin(PluginConfig{Datasource: "auth"})
	if err := p.Configure(context.Background(), nil); err != nil {
		t.Fatalf("configure: %v", err)
	}

	_, err := p.effectivePoolConfig()
	if err == nil {
		t.Fatal("expected an error for a datasource the config-carried binding does not declare")
	}
	if !strings.Contains(err.Error(), configSourceLabel) {
		t.Errorf("error %q should name %s", err.Error(), configSourceLabel)
	}
	if !strings.Contains(err.Error(), "billing") {
		t.Errorf("error %q should list the datasources the document declares", err.Error())
	}
}

// TestPluginConfigure_MalformedConfigSectionErrors surfaces a malformed managed
// document at startup, mirroring how a malformed env document fails at pool
// open.
func TestPluginConfigure_MalformedConfigSectionErrors(t *testing.T) {
	t.Setenv("CONFIG_DATA", `
database:
  databases:
    auth:
      engine: mysql
`)
	p := NewPlugin(PluginConfig{Datasource: "auth"})
	err := p.Configure(context.Background(), nil)
	if err == nil {
		t.Fatal("expected configure to reject a malformed config-section binding")
	}
	if !strings.Contains(err.Error(), configSourceLabel) {
		t.Errorf("error %q should name %s", err.Error(), configSourceLabel)
	}
}

// TestPluginConfigure_NoConfigSectionKeepsEnvPath proves that with no managed
// document in config the env transport resolves exactly as before Configure
// learned about the section.
func TestPluginConfigure_NoConfigSectionKeepsEnvPath(t *testing.T) {
	t.Setenv(EnvBinding, envBindingJSON)
	p := NewPlugin(PluginConfig{Datasource: "auth"})
	if err := p.Configure(context.Background(), nil); err != nil {
		t.Fatalf("configure: %v", err)
	}

	cfg, err := p.effectivePoolConfig()
	if err != nil {
		t.Fatalf("effectivePoolConfig: %v", err)
	}
	if cfg.Connection.Host != "env-db" || cfg.Database != "auth_env" {
		t.Errorf("connection = host %q database %q, want the env binding (env-db/auth_env)", cfg.Connection.Host, cfg.Database)
	}
}

// TestPools_OpenBeforeConfigureUsesConfigSectionBinding pins the lifecycle
// eager-DI ordering invariant: the eager DI build (application Start
// phase 2) resolves the non-lazy *Pools/*Pool singletons and opens the
// default pool BEFORE Configure (phase 3) has resolved the config section.
// The open path must resolve the config-carried document itself — lazily and
// memoized — instead of reading a field only Configure populates; otherwise
// config-only deploys crash-loop while env-transport deploys work.
func TestPools_OpenBeforeConfigureUsesConfigSectionBinding(t *testing.T) {
	t.Setenv(EnvBinding, envBindingJSON)
	t.Setenv("CONFIG_DATA", configSectionYAML)
	p := NewPlugin(PluginConfig{Datasource: "auth"})
	var opened PoolConfig
	p.newPhysical = func(_ context.Context, cfg PoolConfig) (*pgxpool.Pool, error) {
		opened = cfg
		return nil, nil
	}
	pools, err := p.buildPools()
	if err != nil {
		t.Fatalf("buildPools: %v", err)
	}
	// No Configure call: in production the eager container build opens the
	// pool first, so the open must not depend on Configure having run.
	if _, err := pools.For("auth"); err != nil {
		t.Fatalf("pools.For before Configure: %v", err)
	}
	if opened.Connection.Host != "cfg-db" || opened.Database != "auth_cfg" {
		t.Errorf("opened = host %q database %q, want the config-section binding (cfg-db/auth_cfg)", opened.Connection.Host, opened.Database)
	}
}

// TestEffectivePoolConfig_BeforeConfigureUsesConfigSection proves the primary
// single-pool resolution path also reads the config-carried document at open
// time, without Configure having run first.
func TestEffectivePoolConfig_BeforeConfigureUsesConfigSection(t *testing.T) {
	t.Setenv(EnvBinding, envBindingJSON)
	t.Setenv("CONFIG_DATA", configSectionYAML)
	p := NewPlugin(PluginConfig{Datasource: "auth", Pool: PoolConfig{MaxConns: 7}})

	cfg, err := p.effectivePoolConfig()
	if err != nil {
		t.Fatalf("effectivePoolConfig before Configure: %v", err)
	}
	if cfg.Connection.Host != "cfg-db" || cfg.Database != "auth_cfg" {
		t.Errorf("connection = host %q database %q, want the config-section binding (cfg-db/auth_cfg)", cfg.Connection.Host, cfg.Database)
	}
	if cfg.MaxConns != 7 {
		t.Errorf("MaxConns = %d, want the pool's tuning preserved", cfg.MaxConns)
	}
}

// TestPools_OpenBeforeConfigureMalformedSectionFailsLoud proves a malformed
// config section surfaces at pool open — naming the config transport — when
// Configure hasn't had the chance to reject it first, instead of silently
// degrading to the env transport or local fallbacks.
func TestPools_OpenBeforeConfigureMalformedSectionFailsLoud(t *testing.T) {
	t.Setenv(EnvBinding, envBindingJSON)
	t.Setenv("CONFIG_DATA", `
database:
  databases:
    auth:
      engine: mysql
`)
	p := NewPlugin(PluginConfig{Datasource: "auth"})
	p.newPhysical = func(_ context.Context, _ PoolConfig) (*pgxpool.Pool, error) {
		t.Error("a malformed config section must not open a pool")
		return nil, nil
	}
	pools, err := p.buildPools()
	if err != nil {
		t.Fatalf("buildPools: %v", err)
	}
	_, err = pools.For("auth")
	if err == nil {
		t.Fatal("expected the malformed config-section binding to fail the open")
	}
	if !strings.Contains(err.Error(), configSourceLabel) {
		t.Errorf("error %q should name %s", err.Error(), configSourceLabel)
	}
}

// TestPluginProvidesResetsConfigBindingMemoBetweenLifecyclePasses proves the
// config binding memo is scoped to one container build. A Validate or failed
// Start retry collects Provides again; that must re-read config instead of
// keeping a stale nil or error outcome from the previous pass.
func TestPluginProvidesResetsConfigBindingMemoBetweenLifecyclePasses(t *testing.T) {
	t.Setenv(EnvBinding, envBindingJSON)
	p := NewPlugin(PluginConfig{Datasource: "auth"})

	cfg, err := p.effectivePoolConfig()
	if err != nil {
		t.Fatalf("effectivePoolConfig with env fallback: %v", err)
	}
	if cfg.Connection.Host != "env-db" || cfg.Database != "auth_env" {
		t.Fatalf("connection = host %q database %q, want the env binding (env-db/auth_env)", cfg.Connection.Host, cfg.Database)
	}

	t.Setenv("CONFIG_DATA", `
database:
  databases:
    auth:
      engine: mysql
`)
	_ = p.Provides()
	if _, err := p.effectivePoolConfig(); err == nil {
		t.Fatal("expected the next lifecycle pass to re-read and reject the malformed config section")
	} else if !strings.Contains(err.Error(), configSourceLabel) {
		t.Errorf("error %q should name %s", err.Error(), configSourceLabel)
	}

	t.Setenv("CONFIG_DATA", configSectionYAML)
	_ = p.Provides()
	cfg, err = p.effectivePoolConfig()
	if err != nil {
		t.Fatalf("effectivePoolConfig after repaired config: %v", err)
	}
	if cfg.Connection.Host != "cfg-db" || cfg.Database != "auth_cfg" {
		t.Errorf("connection = host %q database %q, want the repaired config-section binding (cfg-db/auth_cfg)", cfg.Connection.Host, cfg.Database)
	}
}

// TestManagedBindingDiag distinguishes the two nil-binding shapes a
// (nil, nil) load produces: an empty databases map — the managed overlay
// resolved to zero datasources (a versioned config-resolve that skipped it) —
// versus no databases map at all (an
// unmanaged workload). The two have different root causes, so the diagnostic
// must read differently.
func TestManagedBindingDiag(t *testing.T) {
	empty := bindingConfigDoc{ProtocolVersion: 1, Databases: map[string]any{}}.managedBindingDiag()
	if !strings.Contains(empty, "empty") || !strings.Contains(empty, "zero datasources") {
		t.Errorf("empty-overlay diag = %q, want it to name an empty databases map / zero datasources", empty)
	}
	absent := bindingConfigDoc{}.managedBindingDiag()
	if !strings.Contains(absent, `no "databases" map`) || strings.Contains(absent, "empty") {
		t.Errorf("no-overlay diag = %q, want it to name a missing databases map (not empty)", absent)
	}
}

// TestPools_NilBindingEmptyOverlayIsSelfDiagnosing proves a Datasources-only
// workload whose resolved "database" section carries an empty databases map —
// the managed overlay resolved to zero datasources — fails the open with an
// error that names that exact gap (not a generic "requires a database
// binding"), so a crash-loop points straight at the config-resolve skip.
func TestPools_NilBindingEmptyOverlayIsSelfDiagnosing(t *testing.T) {
	t.Setenv(EnvBinding, "")
	t.Setenv("CONFIG_DATA", `
database:
  operatorKey: keep-me
  databases: {}
`)
	p := NewPlugin(PluginConfig{Datasources: []DatasourceConfig{{Name: "intelligence_graph"}}})
	opened := false
	p.newPhysical = func(_ context.Context, _ PoolConfig) (*pgxpool.Pool, error) {
		opened = true
		return nil, nil
	}
	pools, err := p.buildPools()
	if err != nil {
		t.Fatalf("buildPools: %v", err)
	}
	_, err = pools.For("intelligence_graph")
	if err == nil {
		t.Fatal("expected a nil-binding open failure for an empty managed overlay")
	}
	if opened {
		t.Error("no pool should open without a resolved binding")
	}
	msg := err.Error()
	for _, want := range []string{"intelligence_graph", "empty", "zero datasources", EnvBinding} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q should name %q so the failure is self-diagnosing", msg, want)
		}
	}
}

// TestPools_NilBindingNoOverlayIsSelfDiagnosing proves the other nil-binding
// shape — a truly unmanaged workload with no databases map and no env binding —
// reads as "no databases map" rather than the empty-overlay wording, so the two
// root causes stay distinguishable in the diagnostic.
func TestPools_NilBindingNoOverlayIsSelfDiagnosing(t *testing.T) {
	t.Setenv(EnvBinding, "")
	t.Setenv("CONFIG_DATA", "")
	p := NewPlugin(PluginConfig{Datasources: []DatasourceConfig{{Name: "auth"}}})
	pools, err := p.buildPools()
	if err != nil {
		t.Fatalf("buildPools: %v", err)
	}
	_, err = pools.For("auth")
	if err == nil {
		t.Fatal("expected a nil-binding open failure for an unmanaged workload")
	}
	msg := err.Error()
	if !strings.Contains(msg, "auth") || !strings.Contains(msg, `no "databases" map`) {
		t.Errorf("error %q should name the datasource and the missing databases map", msg)
	}
	if strings.Contains(msg, "empty") {
		t.Errorf("error %q must not use the empty-overlay wording for an unmanaged workload", msg)
	}
}

// TestEffectivePoolConfig_NilBindingPrimaryIsSelfDiagnosing proves the primary
// single-pool path folds the same diagnostic into its nil-binding failure, so a
// single-datasource workload with an empty managed overlay and no fallback also
// names the gap instead of failing generically.
func TestEffectivePoolConfig_NilBindingPrimaryIsSelfDiagnosing(t *testing.T) {
	t.Setenv(EnvBinding, "")
	t.Setenv("CONFIG_DATA", `
database:
  databases: {}
`)
	p := NewPlugin(PluginConfig{Datasource: "auth"})
	_, err := p.effectivePoolConfig()
	if err == nil {
		t.Fatal("expected a nil-binding failure for an empty managed overlay")
	}
	if msg := err.Error(); !strings.Contains(msg, "empty") || !strings.Contains(msg, "zero datasources") {
		t.Errorf("error %q should name the empty managed overlay", msg)
	}
}

// TestPools_OpenUsesConfigSectionBinding proves the lazily-opened *Pools path
// reads the config-resolved document through the registry seam, so named
// datasources resolve from config exactly like the primary.
func TestPools_OpenUsesConfigSectionBinding(t *testing.T) {
	t.Setenv(EnvBinding, envBindingJSON)
	t.Setenv("CONFIG_DATA", configSectionYAML)
	p := NewPlugin(PluginConfig{Datasource: "auth"})
	var opened PoolConfig
	p.newPhysical = func(_ context.Context, cfg PoolConfig) (*pgxpool.Pool, error) {
		opened = cfg
		return nil, nil
	}
	// Build the registry before Configure resolves the section, mirroring a DI
	// factory that runs early: the open below must still see the document.
	pools, err := p.buildPools()
	if err != nil {
		t.Fatalf("buildPools: %v", err)
	}
	if err := p.Configure(context.Background(), nil); err != nil {
		t.Fatalf("configure: %v", err)
	}

	if _, err := pools.For("auth"); err != nil {
		t.Fatalf("pools.For: %v", err)
	}
	if opened.Connection.Host != "cfg-db" || opened.Database != "auth_cfg" {
		t.Errorf("opened = host %q database %q, want the config-section binding (cfg-db/auth_cfg)", opened.Connection.Host, opened.Database)
	}
}
