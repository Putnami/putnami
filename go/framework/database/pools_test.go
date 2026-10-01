package database

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.putnami.dev/app"
	"go.putnami.dev/inject"
	"go.putnami.dev/protocol/infra"
	protocolmigration "go.putnami.dev/protocol/migration"
)

// multiBinding declares three datasources on (logically) one platform database,
// each owning a distinct schema, so a resolved pool's search_path identifies
// which datasource it was opened for.
const multiBinding = `{
	"protocolVersion": 1,
	"databases": {
		"platform_core":    { "engine": "postgres", "schema": "core",    "connection": { "host": "core-db",    "database": "platform", "user": "u", "password": "p", "ssl": false } },
		"platform_iam":     { "engine": "postgres", "schema": "iam",     "connection": { "host": "iam-db",     "database": "platform", "user": "u", "password": "p", "ssl": false } },
		"platform_billing": { "engine": "postgres", "schema": "billing", "connection": { "host": "billing-db", "database": "platform", "user": "u", "password": "p", "ssl": false } }
	}
}`

// recordingPoolFactory is a physical-open seam that records every PoolConfig it
// is asked to open and returns no live connection (a nil *pgxpool.Pool the
// package tolerates), so tests can assert lazy opening, physical sharing, and
// the resolved config without a database. Each call is one PHYSICAL open: two
// datasources sharing a connection identity reach it once.
type recordingPoolFactory struct {
	mu    sync.Mutex
	calls []PoolConfig
}

func (f *recordingPoolFactory) New(_ context.Context, cfg PoolConfig) (*pgxpool.Pool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, cfg)
	return nil, nil
}

func (f *recordingPoolFactory) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// blockingPoolFactory is a physical-open seam that announces each open on
// entered and then blocks on release, so a test can prove two opens are in
// flight at once — which is impossible if a single lock is held across the
// connect — or that two datasources of one identity reach it once.
type blockingPoolFactory struct {
	entered chan PoolConfig
	release chan struct{}
	mu      sync.Mutex
	calls   int
}

func (f *blockingPoolFactory) New(_ context.Context, cfg PoolConfig) (*pgxpool.Pool, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	f.entered <- cfg
	<-f.release
	return nil, nil
}

func (f *blockingPoolFactory) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// newTestPools builds the plugin's *Pools registry wired to a recording factory,
// also exercising buildPools and poolSpecs.
func newTestPools(t *testing.T, cfg PluginConfig) (*Pools, *recordingPoolFactory) {
	t.Helper()
	p := NewPlugin(cfg)
	f := &recordingPoolFactory{}
	p.newPhysical = f.New
	pools, err := p.buildPools()
	if err != nil {
		t.Fatalf("buildPools: %v", err)
	}
	return pools, f
}

// TestPools_For_ResolvesNamedDatasourceFromBinding proves each named datasource
// gets its own pool, opened lazily on first For, carrying the binding's
// search_path and the entry's own tuning, and cached thereafter.
func TestPools_For_ResolvesNamedDatasourceFromBinding(t *testing.T) {
	t.Setenv(EnvBinding, multiBinding)
	pools, f := newTestPools(t, PluginConfig{
		Datasource: "platform_core",
		Datasources: []DatasourceConfig{
			{Name: "platform_iam", Pool: PoolConfig{MaxConns: 13}},
			{Name: "platform_billing"},
		},
	})

	if got := f.count(); got != 0 {
		t.Fatalf("registry must open nothing until For/Default, got %d opens", got)
	}

	iam, err := pools.For("platform_iam")
	if err != nil {
		t.Fatalf("For(platform_iam): %v", err)
	}
	if iam.cfg.SearchPath != "iam" {
		t.Errorf("iam search_path = %q, want iam (from binding)", iam.cfg.SearchPath)
	}
	if iam.cfg.MaxConns != 13 {
		t.Errorf("iam MaxConns = %d, want 13 (per-datasource tuning preserved)", iam.cfg.MaxConns)
	}
	if f.count() != 1 {
		t.Fatalf("first For should open exactly one pool, got %d", f.count())
	}

	// A second For returns the cached instance without opening again.
	if iam2, err := pools.For("platform_iam"); err != nil {
		t.Fatalf("For(platform_iam) again: %v", err)
	} else if iam2 != iam {
		t.Error("For must return the cached pool instance")
	}
	if f.count() != 1 {
		t.Errorf("cached For must not open a new pool, got %d opens", f.count())
	}

	// A different datasource opens its own pool with its own search_path.
	billing, err := pools.For("platform_billing")
	if err != nil {
		t.Fatalf("For(platform_billing): %v", err)
	}
	if billing == iam {
		t.Error("distinct datasources must get distinct pools")
	}
	if billing.cfg.SearchPath != "billing" {
		t.Errorf("billing search_path = %q, want billing", billing.cfg.SearchPath)
	}
	if f.count() != 2 {
		t.Errorf("want 2 pools opened, got %d", f.count())
	}
}

// TestPools_For_NamedDatasourceFallsBackToLocalPool proves additional named
// datasources get the same local DSN/SearchPath fallback as the primary when no
// deploy binding is injected.
func TestPools_For_NamedDatasourceFallsBackToLocalPool(t *testing.T) {
	t.Setenv(EnvBinding, "")
	pools, f := newTestPools(t, PluginConfig{
		Datasources: []DatasourceConfig{
			{
				Name: "platform_iam",
				Pool: PoolConfig{
					DSN:        "postgres://local/platform",
					SearchPath: "iam",
					MaxConns:   13,
				},
			},
		},
	})

	iam, err := pools.For("platform_iam")
	if err != nil {
		t.Fatalf("For(platform_iam): %v", err)
	}
	if iam.cfg.DSN != "postgres://local/platform" {
		t.Errorf("iam DSN = %q, want local fallback DSN", iam.cfg.DSN)
	}
	if iam.cfg.SearchPath != "iam" {
		t.Errorf("iam search_path = %q, want local fallback search_path", iam.cfg.SearchPath)
	}
	if iam.cfg.MaxConns != 13 {
		t.Errorf("iam MaxConns = %d, want per-datasource tuning preserved", iam.cfg.MaxConns)
	}
	if f.count() != 1 {
		t.Errorf("want 1 pool opened, got %d", f.count())
	}
}

// TestPools_For_NamedDatasourceStructuredConnectionUsesNameAsDatabase proves a
// named datasource's structured local Connection gets a database target from the
// datasource name when no deploy binding is injected.
func TestPools_For_NamedDatasourceStructuredConnectionUsesNameAsDatabase(t *testing.T) {
	t.Setenv(EnvBinding, "")
	pools, f := newTestPools(t, PluginConfig{
		Datasources: []DatasourceConfig{
			{
				Name: "platform_iam",
				Pool: PoolConfig{
					Connection: Connection{Host: "localhost", User: "u", Password: "p"},
					SearchPath: "iam",
					MaxConns:   13,
				},
			},
		},
	})

	iam, err := pools.For("platform_iam")
	if err != nil {
		t.Fatalf("For(platform_iam): %v", err)
	}
	if iam.cfg.Database != "platform_iam" {
		t.Errorf("iam Database = %q, want the datasource name platform_iam", iam.cfg.Database)
	}
	dsn, err := iam.cfg.resolveDSN()
	if err != nil {
		t.Fatalf("resolveDSN: %v", err)
	}
	if !strings.Contains(dsn, "dbname=platform_iam") {
		t.Errorf("iam dsn = %q, want the datasource name as the database target", dsn)
	}
	if iam.cfg.SearchPath != "iam" {
		t.Errorf("iam search_path = %q, want local fallback search_path", iam.cfg.SearchPath)
	}
	if f.count() != 1 {
		t.Errorf("want 1 pool opened, got %d", f.count())
	}
}

// TestPools_For_NamedDatasourceWithoutBindingOrLocalPoolErrors keeps a clear
// startup error for a declared datasource with no injected binding and no local
// Pool connection fallback.
func TestPools_For_NamedDatasourceWithoutBindingOrLocalPoolErrors(t *testing.T) {
	t.Setenv(EnvBinding, "")
	pools, _ := newTestPools(t, PluginConfig{Datasources: []DatasourceConfig{{Name: "platform_iam"}}})

	_, err := pools.For("platform_iam")
	if err == nil {
		t.Fatal("expected an error for a named datasource with no binding or local connection")
	}
	if !strings.Contains(err.Error(), EnvBinding) || !strings.Contains(err.Error(), "Pool") {
		t.Errorf("error %q should mention %s and the local Pool fallback", err.Error(), EnvBinding)
	}
}

// TestPools_Default_SharesNamedPrimaryInstance proves the default datasource and
// For(primary name) resolve to one shared pool.
func TestPools_Default_SharesNamedPrimaryInstance(t *testing.T) {
	t.Setenv(EnvBinding, multiBinding)
	pools, f := newTestPools(t, PluginConfig{
		Datasource:  "platform_core",
		Datasources: []DatasourceConfig{{Name: "platform_iam"}},
	})

	dflt, err := pools.Default()
	if err != nil {
		t.Fatalf("Default: %v", err)
	}
	if dflt.cfg.SearchPath != "core" {
		t.Errorf("default search_path = %q, want core", dflt.cfg.SearchPath)
	}
	byName, err := pools.For("platform_core")
	if err != nil {
		t.Fatalf("For(platform_core): %v", err)
	}
	if byName != dflt {
		t.Error("Default and For(primary name) must be the same instance")
	}
	if f.count() != 1 {
		t.Errorf("resolving the primary twice must open one pool, got %d", f.count())
	}
}

// TestPools_Default_LegacyExplicitPrimary proves an explicit-connection primary
// (no datasource name) is keyed under the canonical default datasource.
func TestPools_Default_LegacyExplicitPrimary(t *testing.T) {
	pools, _ := newTestPools(t, PluginConfig{Pool: PoolConfig{DSN: "postgres://x/y"}})

	dflt, err := pools.Default()
	if err != nil {
		t.Fatalf("Default: %v", err)
	}
	if dflt.cfg.DSN != "postgres://x/y" {
		t.Errorf("default DSN = %q, want the explicit DSN", dflt.cfg.DSN)
	}
	byDefault, err := pools.For(protocolmigration.DefaultDatasource)
	if err != nil {
		t.Fatalf("For(%q): %v", protocolmigration.DefaultDatasource, err)
	}
	if byDefault != dflt {
		t.Error("the explicit-connection primary must resolve identically via Default and the default name")
	}
}

// TestPools_Default_NoneWhenDatasourcesOnly proves a workload that declares only
// named datasources has no default and is told to use For.
func TestPools_Default_NoneWhenDatasourcesOnly(t *testing.T) {
	pools, _ := newTestPools(t, PluginConfig{Datasources: []DatasourceConfig{{Name: "platform_iam"}}})
	_, err := pools.Default()
	if err == nil {
		t.Fatal("expected an error: a Datasources-only workload has no default datasource")
	}
	if !strings.Contains(err.Error(), "platform_iam") {
		t.Errorf("error %q should list the declared datasources", err.Error())
	}
}

// TestPools_For_UnknownDatasource proves an undeclared name fails closed, listing
// what the workload does declare.
func TestPools_For_UnknownDatasource(t *testing.T) {
	pools, _ := newTestPools(t, PluginConfig{Datasources: []DatasourceConfig{{Name: "platform_iam"}}})
	_, err := pools.For("nope")
	if err == nil {
		t.Fatal("expected an error for an unknown datasource")
	}
	if !strings.Contains(err.Error(), "platform_iam") {
		t.Errorf("error %q should list declared datasources", err.Error())
	}
}

// TestPools_For_EmptyName rejects a blank datasource name.
func TestPools_For_EmptyName(t *testing.T) {
	pools, _ := newTestPools(t, PluginConfig{Pool: PoolConfig{DSN: "postgres://x/y"}})
	if _, err := pools.For("  "); err == nil {
		t.Fatal("expected an error for a blank datasource name")
	}
}

// TestPools_Names lists every declared datasource, sorted, without opening any.
func TestPools_Names(t *testing.T) {
	pools, f := newTestPools(t, PluginConfig{
		Datasource:  "platform_core",
		Datasources: []DatasourceConfig{{Name: "platform_iam"}, {Name: "platform_billing"}},
	})
	got := pools.Names()
	want := []string{"platform_billing", "platform_core", "platform_iam"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Names() = %v, want %v", got, want)
	}
	if f.count() != 0 {
		t.Errorf("Names must not open pools, got %d opens", f.count())
	}
}

// TestPools_Close_ClearsCacheAndIsIdempotent proves Close releases opened pools
// (a later For re-opens) and a second Close is a no-op.
func TestPools_Close_ClearsCacheAndIsIdempotent(t *testing.T) {
	t.Setenv(EnvBinding, multiBinding)
	pools, f := newTestPools(t, PluginConfig{Datasources: []DatasourceConfig{{Name: "platform_iam"}}})

	if _, err := pools.For("platform_iam"); err != nil {
		t.Fatalf("For: %v", err)
	}
	if f.count() != 1 {
		t.Fatalf("want 1 open, got %d", f.count())
	}

	pools.Close()
	pools.Close() // idempotent: must not panic or double-close

	if _, err := pools.For("platform_iam"); err != nil {
		t.Fatalf("For after Close: %v", err)
	}
	if f.count() != 2 {
		t.Errorf("Close must clear the cache so For re-opens (want 2 opens), got %d", f.count())
	}
}

// TestPools_HealthCheck_NoDefaultNoOpened proves a named-only registry with
// nothing opened reports healthy (no single pool to certify, none in use).
func TestPools_HealthCheck_NoDefaultNoOpened(t *testing.T) {
	pools, _ := newTestPools(t, PluginConfig{Datasources: []DatasourceConfig{{Name: "platform_iam"}}})
	if err := pools.HealthCheck(context.Background()); err != nil {
		t.Errorf("HealthCheck with no default and nothing opened = %v, want nil", err)
	}
}

// TestPoolSpecs_Errors covers the configuration errors poolSpecs rejects.
func TestPoolSpecs_Errors(t *testing.T) {
	if _, _, err := (PluginConfig{Datasources: []DatasourceConfig{{Name: "  "}}}).poolSpecs(); err == nil {
		t.Error("expected an error for a blank Datasources name")
	}
	if _, _, err := (PluginConfig{Datasource: "iam", Datasources: []DatasourceConfig{{Name: "iam"}}}).poolSpecs(); err == nil {
		t.Error("expected an error when a Datasources name collides with the primary")
	}
	if _, _, err := (PluginConfig{Datasources: []DatasourceConfig{{Name: "iam"}, {Name: "iam"}}}).poolSpecs(); err == nil {
		t.Error("expected an error for duplicate Datasources names")
	}
}

// TestPlugin_UnnamedPoolDelegatesToRegistryDefault wires the plugin's providers
// into a container and proves the unnamed *Pool token resolves to the registry's
// default instance — the backward-compatible single-pool surface, now backed by
// the registry.
func TestPlugin_UnnamedPoolDelegatesToRegistryDefault(t *testing.T) {
	t.Setenv(EnvBinding, multiBinding)
	p := NewPlugin(PluginConfig{
		Datasource:  "platform_core",
		Datasources: []DatasourceConfig{{Name: "platform_iam"}},
	})
	f := &recordingPoolFactory{}
	p.newPhysical = f.New

	c := inject.NewContainer("test", nil)
	for _, reg := range p.Provides() {
		if err := c.Register(reg); err != nil {
			t.Fatalf("register: %v", err)
		}
	}

	poolsVal, err := c.Get(inject.TokenOf[*Pools]())
	if err != nil {
		t.Fatalf("get *Pools: %v", err)
	}
	pools := poolsVal.(*Pools)

	poolVal, err := c.Get(inject.TokenOf[*Pool]())
	if err != nil {
		t.Fatalf("get *Pool: %v", err)
	}
	pool := poolVal.(*Pool)

	dflt, err := pools.Default()
	if err != nil {
		t.Fatalf("Default: %v", err)
	}
	if pool != dflt {
		t.Error("the unnamed *Pool must be the registry's default instance")
	}
	if pool.cfg.SearchPath != "core" {
		t.Errorf("unnamed *Pool search_path = %q, want core", pool.cfg.SearchPath)
	}
}

// TestPlugin_DatasourcesOnly_NoUnnamedPool proves a workload that declares only
// named datasources exposes *Pools but not the unnamed *Pool token.
func TestPlugin_DatasourcesOnly_NoUnnamedPool(t *testing.T) {
	t.Setenv(EnvBinding, multiBinding)
	p := NewPlugin(PluginConfig{Datasources: []DatasourceConfig{{Name: "platform_iam"}}})
	f := &recordingPoolFactory{}
	p.newPhysical = f.New

	c := inject.NewContainer("test", nil)
	for _, reg := range p.Provides() {
		if err := c.Register(reg); err != nil {
			t.Fatalf("register: %v", err)
		}
	}

	if _, err := c.Get(inject.TokenOf[*Pool]()); err == nil {
		t.Error("a Datasources-only workload must not provide the unnamed *Pool")
	}
	poolsVal, err := c.Get(inject.TokenOf[*Pools]())
	if err != nil {
		t.Fatalf("get *Pools: %v", err)
	}
	if _, err := poolsVal.(*Pools).For("platform_iam"); err != nil {
		t.Errorf("named datasource must still resolve via *Pools: %v", err)
	}
}

// TestPluginDescribe_MultipleDatasourcesEmitInfra proves the build emits an infra
// requirement for every named datasource — the primary plus each Datasources
// entry — so each datasource-bound runtime pool is provisionable and bindable.
func TestPluginDescribe_MultipleDatasourcesEmitInfra(t *testing.T) {
	out := t.TempDir()
	p := NewPlugin(PluginConfig{
		Datasource:  "platform_core",
		Datasources: []DatasourceConfig{{Name: "platform_iam"}, {Name: "platform_billing"}},
	})

	if err := p.Describe(&app.DescribeContext{OutputDir: out, Targets: []string{"all"}}); err != nil {
		t.Fatalf("describe: %v", err)
	}

	manifest, diags := infra.LoadPerProjectManifest(infra.SidecarPathIn(out, "database"))
	if d := firstError(diags); d != "" {
		t.Fatalf("loaded sidecar has errors: %s", d)
	}
	if manifest == nil {
		t.Fatal("expected a manifest")
	}
	if len(manifest.Databases) != 3 {
		t.Fatalf("expected 3 databases, got %d: %+v", len(manifest.Databases), manifest.Databases)
	}
	got := map[string]infra.Engine{}
	for _, d := range manifest.Databases {
		got[d.Name] = d.Engine
	}
	for _, name := range []string{"platform_core", "platform_iam", "platform_billing"} {
		if got[name] != infra.EnginePostgres {
			t.Errorf("datasource %q missing or wrong engine in manifest: %+v", name, manifest.Databases)
		}
	}
}

// TestPools_For_OpensDistinctDatasourcesConcurrently proves opening one
// datasource does not block opening another: the registry must not hold its
// lock across the physical connect. With a blocking factory, two For calls for
// distinct datasources must both reach the factory before either is released —
// which they cannot if first-opens are serialized behind a single lock.
func TestPools_For_OpensDistinctDatasourcesConcurrently(t *testing.T) {
	t.Setenv(EnvBinding, multiBinding)
	f := &blockingPoolFactory{entered: make(chan PoolConfig, 2), release: make(chan struct{})}
	p := NewPlugin(PluginConfig{Datasources: []DatasourceConfig{{Name: "platform_iam"}, {Name: "platform_billing"}}})
	p.newPhysical = f.New
	pools, err := p.buildPools()
	if err != nil {
		t.Fatalf("buildPools: %v", err)
	}

	var wg sync.WaitGroup
	pool := make([]*Pool, 2)
	errs := make([]error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); pool[0], errs[0] = pools.For("platform_iam") }()
	go func() { defer wg.Done(); pool[1], errs[1] = pools.For("platform_billing") }()

	// Both opens must be in flight at once. If For serialized them, only one
	// would enter the factory and the second receive would time out.
	seen := map[string]bool{}
	for range 2 {
		select {
		case cfg := <-f.entered:
			seen[cfg.SearchPath] = true
		case <-time.After(2 * time.Second):
			t.Fatal("a second datasource open was blocked behind the first: the registry holds its lock across connect")
		}
	}
	close(f.release)
	wg.Wait()

	for i := range 2 {
		if errs[i] != nil {
			t.Fatalf("For #%d: %v", i, errs[i])
		}
	}
	if !seen["iam"] || !seen["billing"] {
		t.Errorf("expected both iam and billing to open, saw %v", seen)
	}
	if pool[0] == pool[1] {
		t.Error("distinct datasources must get distinct pools")
	}
}

// TestPools_For_ConcurrentSameDatasourceOpensOnce proves concurrent first opens
// of the same datasource collapse to a single connect and share the instance.
func TestPools_For_ConcurrentSameDatasourceOpensOnce(t *testing.T) {
	t.Setenv(EnvBinding, multiBinding)
	f := &blockingPoolFactory{entered: make(chan PoolConfig, 2), release: make(chan struct{})}
	p := NewPlugin(PluginConfig{Datasources: []DatasourceConfig{{Name: "platform_iam"}}})
	p.newPhysical = f.New
	pools, err := p.buildPools()
	if err != nil {
		t.Fatalf("buildPools: %v", err)
	}

	var wg sync.WaitGroup
	pool := make([]*Pool, 2)
	errs := make([]error, 2)
	wg.Add(2)
	for i := range pool {
		go func(i int) { defer wg.Done(); pool[i], errs[i] = pools.For("platform_iam") }(i)
	}

	// Exactly one open reaches the factory; the other waits on it.
	select {
	case <-f.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("no open reached the factory")
	}
	close(f.release)
	wg.Wait()

	for i := range pool {
		if errs[i] != nil {
			t.Fatalf("For #%d: %v", i, errs[i])
		}
	}
	if pool[0] != pool[1] {
		t.Error("concurrent opens of one datasource must share a single pool")
	}
	if f.count() != 1 {
		t.Errorf("concurrent first opens must connect once, got %d", f.count())
	}
}
