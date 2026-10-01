package database

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	perrors "go.putnami.dev/errors"
	"go.putnami.dev/protocol/features/spectest"
)

// sharedBinding declares three datasources over TWO physical databases:
// platform_core and platform_billing own two schemas of the same platform
// database (identical connections), analytics_events owns a schema of another
// one. It is the shape a workload reading several schemas of one database
// declares, and the shape that used to open one pool per datasource.
const sharedBinding = `{
	"protocolVersion": 1,
	"databases": {
		"platform_core":    { "engine": "postgres", "schema": "core",    "connection": { "host": "platform-db",  "database": "platform",  "user": "u", "password": "p", "ssl": false } },
		"platform_billing": { "engine": "postgres", "schema": "billing", "connection": { "host": "platform-db",  "database": "platform",  "user": "u", "password": "p", "ssl": false } },
		"analytics_events": { "engine": "postgres", "schema": "events",  "connection": { "host": "analytics-db", "database": "analytics", "user": "u", "password": "p", "ssl": false } }
	}
}`

// TestPools_SameConnectionIdentitySharesOnePhysicalPool proves the registry
// keys physical pools by connection identity, not by datasource: two datasources
// whose effective connections are byte-identical share one physical pool (one
// physical open) while keeping distinct logical pools with their own
// search_path, and a datasource on another database opens its own.
func TestPools_SameConnectionIdentitySharesOnePhysicalPool(t *testing.T) {
	spectest.Proves(t, "go/shared-datasource-pooling", "one-pool-per-physical-database", "same-connection-identity-shares-one-physical-pool")
	spectest.Proves(t, "go/shared-datasource-pooling", "one-pool-per-physical-database", "distinct-connection-identities-open-distinct-pools")
	t.Setenv(EnvBinding, sharedBinding)
	pools, f := newTestPools(t, PluginConfig{
		Datasource:  "platform_core",
		Datasources: []DatasourceConfig{{Name: "platform_billing"}, {Name: "analytics_events"}},
	})

	core, err := pools.For("platform_core")
	if err != nil {
		t.Fatalf("For(platform_core): %v", err)
	}
	billing, err := pools.For("platform_billing")
	if err != nil {
		t.Fatalf("For(platform_billing): %v", err)
	}
	events, err := pools.For("analytics_events")
	if err != nil {
		t.Fatalf("For(analytics_events): %v", err)
	}

	if got := f.count(); got != 2 {
		t.Errorf("physical opens = %d, want 2 (one per physical database, not one per datasource)", got)
	}
	if got := pools.PhysicalCount(); got != 2 {
		t.Errorf("PhysicalCount() = %d, want 2", got)
	}
	if core == billing {
		t.Fatal("two datasources must get distinct logical pools even when they share a physical pool")
	}
	if core.physical != billing.physical {
		t.Error("platform_core and platform_billing resolve to one connection and must share the physical pool")
	}
	if events.physical == core.physical {
		t.Error("analytics_events connects elsewhere and must not share the platform physical pool")
	}
	if core.cfg.SearchPath != "core" || billing.cfg.SearchPath != "billing" || events.cfg.SearchPath != "events" {
		t.Errorf("search_paths = %q/%q/%q, want core/billing/events: each logical pool keeps its own", core.cfg.SearchPath, billing.cfg.SearchPath, events.cfg.SearchPath)
	}
	// Each logical pool marks its acquires with its own search_path, which is
	// what keeps the shared connections honest (see search_path.go).
	if got, _ := searchPathRequestFrom(core.acquireContext(context.Background())); got != "core" {
		t.Errorf("platform_core acquire request = %q, want core", got)
	}
	if got, _ := searchPathRequestFrom(billing.acquireContext(context.Background())); got != "billing" {
		t.Errorf("platform_billing acquire request = %q, want billing", got)
	}
}

// TestPools_ConcurrentFirstOpensOfOneIdentityConnectOnce proves two datasources
// racing to open the same physical database collapse to one connect: the
// second waits on the first's in-flight connect and then shares the pool.
func TestPools_ConcurrentFirstOpensOfOneIdentityConnectOnce(t *testing.T) {
	spectest.Proves(t, "go/shared-datasource-pooling", "one-pool-per-physical-database", "concurrent-first-opens-connect-once")
	t.Setenv(EnvBinding, sharedBinding)
	f := &blockingPoolFactory{entered: make(chan PoolConfig, 2), release: make(chan struct{})}
	p := NewPlugin(PluginConfig{Datasources: []DatasourceConfig{{Name: "platform_core"}, {Name: "platform_billing"}}})
	p.newPhysical = f.New
	pools, err := p.buildPools()
	if err != nil {
		t.Fatalf("buildPools: %v", err)
	}

	var wg sync.WaitGroup
	got := make([]*Pool, 2)
	errs := make([]error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); got[0], errs[0] = pools.For("platform_core") }()
	go func() { defer wg.Done(); got[1], errs[1] = pools.For("platform_billing") }()

	// Exactly one connect reaches the factory; the other datasource waits on it.
	select {
	case <-f.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("no physical open reached the factory")
	}
	select {
	case cfg := <-f.entered:
		t.Fatalf("a second physical open (search_path %q) reached the factory: concurrent first opens of one identity must connect once", cfg.SearchPath)
	case <-time.After(50 * time.Millisecond):
	}
	close(f.release)
	wg.Wait()

	for i := range got {
		if errs[i] != nil {
			t.Fatalf("For #%d: %v", i, errs[i])
		}
	}
	if f.count() != 1 {
		t.Errorf("physical opens = %d, want 1", f.count())
	}
	if got[0] == got[1] || got[0].physical != got[1].physical {
		t.Error("the two datasources must be distinct logical pools over one shared physical pool")
	}
	if pools.PhysicalCount() != 1 {
		t.Errorf("PhysicalCount() = %d, want 1", pools.PhysicalCount())
	}
}

// TestPools_TuningMismatchOnOneDatabaseIsADeclarationError proves a datasource
// whose tuning differs from the tuning its shared physical pool was created
// with fails to open with a db.datasource error naming both datasources and the
// differing field — the registry never takes the max and never opens a second
// pool.
func TestPools_TuningMismatchOnOneDatabaseIsADeclarationError(t *testing.T) {
	spectest.Proves(t, "go/shared-datasource-pooling", "tuning-agreement", "different-tuning-on-one-database-is-a-declaration-error")
	t.Setenv(EnvBinding, sharedBinding)
	pools, f := newTestPools(t, PluginConfig{
		Datasource: "platform_core",
		Datasources: []DatasourceConfig{
			{Name: "platform_billing", Pool: PoolConfig{MaxConns: 4, MaxConnIdleTime: 5 * time.Second}},
		},
	})

	if _, err := pools.For("platform_core"); err != nil {
		t.Fatalf("For(platform_core): %v", err)
	}
	_, err := pools.For("platform_billing")
	if err == nil {
		t.Fatal("expected a declaration error: platform_billing declares a different tuning for the platform database")
	}
	msg := err.Error()
	for _, want := range []string{"platform_core", "platform_billing", "MaxConns 10 vs 4", "MaxConnIdleTime 30m0s vs 5s", "declare it identically"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q should contain %q", msg, want)
		}
	}
	if strings.Contains(msg, "platform-db") || strings.Contains(msg, "password") {
		t.Errorf("error %q must not carry the connection identity", msg)
	}
	if f.count() != 1 || pools.PhysicalCount() != 1 {
		t.Errorf("physical opens/count = %d/%d, want 1/1: a mismatch must not open a second pool", f.count(), pools.PhysicalCount())
	}
	if !strings.Contains(msg, string(CodeDatasource)) {
		t.Errorf("error %q should carry the %s code", msg, CodeDatasource)
	}
}

// TestPools_HookMismatchOnOneDatabaseIsADeclarationError proves the identity
// hooks are part of the agreement: the same top-level function on every
// datasource is fine (Cloud's shape), a different function or a nil on one side
// is the declaration error naming the hook.
func TestPools_HookMismatchOnOneDatabaseIsADeclarationError(t *testing.T) {
	spectest.Proves(t, "go/shared-datasource-pooling", "tuning-agreement", "different-identity-hooks-on-one-database-is-a-declaration-error")
	t.Setenv(EnvBinding, sharedBinding)

	t.Run("same functions agree", func(t *testing.T) {
		pools, f := newTestPools(t, PluginConfig{
			Datasource: "platform_core",
			Pool:       PoolConfig{IdentityResolver: fakeIdentity, TokenFetcher: fakeToken},
			Datasources: []DatasourceConfig{
				{Name: "platform_billing", Pool: PoolConfig{IdentityResolver: fakeIdentity, TokenFetcher: fakeToken}},
			},
		})
		if _, err := pools.For("platform_core"); err != nil {
			t.Fatalf("For(platform_core): %v", err)
		}
		if _, err := pools.For("platform_billing"); err != nil {
			t.Fatalf("For(platform_billing) with the same hooks: %v", err)
		}
		if f.count() != 1 {
			t.Errorf("physical opens = %d, want 1", f.count())
		}
	})

	t.Run("different resolver and missing fetcher disagree", func(t *testing.T) {
		pools, _ := newTestPools(t, PluginConfig{
			Datasource: "platform_core",
			Pool:       PoolConfig{IdentityResolver: fakeIdentity, TokenFetcher: fakeToken},
			Datasources: []DatasourceConfig{
				{Name: "platform_billing", Pool: PoolConfig{IdentityResolver: otherIdentity}},
			},
		})
		if _, err := pools.For("platform_core"); err != nil {
			t.Fatalf("For(platform_core): %v", err)
		}
		_, err := pools.For("platform_billing")
		if err == nil {
			t.Fatal("expected a declaration error for differing identity hooks")
		}
		msg := err.Error()
		for _, want := range []string{"platform_core", "platform_billing", "IdentityResolver", "TokenFetcher"} {
			if !strings.Contains(msg, want) {
				t.Errorf("error %q should name %q", msg, want)
			}
		}
	})
}

func fakeIdentity(context.Context) (string, error)  { return "app@example.iam", nil }
func otherIdentity(context.Context) (string, error) { return "other@example.iam", nil }
func fakeToken(context.Context) (string, error)     { return "token", nil }

// TestPools_SharedPhysicalPoolLifetime proves the reference-counted lifetime:
// closing one datasource's logical pool leaves the shared physical pool open
// for the other, closing the last handle closes it, and Pools.Close stays
// idempotent with a later For re-opening.
func TestPools_SharedPhysicalPoolLifetime(t *testing.T) {
	spectest.Proves(t, "go/shared-datasource-pooling", "shared-pool-lifetime", "closing-one-datasource-keeps-the-shared-pool-open")
	spectest.Proves(t, "go/shared-datasource-pooling", "shared-pool-lifetime", "closing-the-last-datasource-closes-the-shared-pool")
	spectest.Proves(t, "go/shared-datasource-pooling", "shared-pool-lifetime", "registry-close-is-idempotent-and-reopens")
	t.Setenv(EnvBinding, sharedBinding)
	pools, f := newTestPools(t, PluginConfig{
		Datasource:  "platform_core",
		Datasources: []DatasourceConfig{{Name: "platform_billing"}},
	})

	core, err := pools.For("platform_core")
	if err != nil {
		t.Fatalf("For(platform_core): %v", err)
	}
	billing, err := pools.For("platform_billing")
	if err != nil {
		t.Fatalf("For(platform_billing): %v", err)
	}
	shared := core.physical
	if shared != billing.physical {
		t.Fatal("precondition: both datasources share one physical pool")
	}

	core.Close()
	core.Close() // idempotent per handle: must not release billing's reference
	if shared.isClosed() {
		t.Fatal("closing platform_core must leave the shared physical pool open for platform_billing")
	}
	if pools.PhysicalCount() != 1 {
		t.Errorf("PhysicalCount() after one logical close = %d, want 1", pools.PhysicalCount())
	}

	billing.Close()
	if !shared.isClosed() {
		t.Fatal("closing the last datasource must close the shared physical pool")
	}
	if pools.PhysicalCount() != 0 {
		t.Errorf("PhysicalCount() after the last logical close = %d, want 0", pools.PhysicalCount())
	}

	// Registry close: every opened logical pool is released, the caches drop,
	// a second Close is a no-op, and For re-opens (a new physical connect).
	pools.Close()
	pools.Close()
	reopened, err := pools.For("platform_billing")
	if err != nil {
		t.Fatalf("For after Close: %v", err)
	}
	if reopened == billing || reopened.physical == shared {
		t.Error("For after Close must re-open a fresh logical and physical pool")
	}
	if f.count() != 2 {
		t.Errorf("physical opens = %d, want 2 (the initial shared open plus the re-open)", f.count())
	}
	if pools.PhysicalCount() != 1 {
		t.Errorf("PhysicalCount() after re-open = %d, want 1", pools.PhysicalCount())
	}
	pools.Close()
	if pools.PhysicalCount() != 0 {
		t.Errorf("PhysicalCount() after the final Close = %d, want 0", pools.PhysicalCount())
	}
}

// TestPools_RegistryCloseReleasesEveryHandleOnce proves Pools.Close over two
// datasources sharing one physical pool closes that pool exactly when both
// handles are released, and that a standalone-style single handle closes on its
// own Close.
func TestPools_RegistryCloseReleasesEveryHandleOnce(t *testing.T) {
	t.Setenv(EnvBinding, sharedBinding)
	pools, _ := newTestPools(t, PluginConfig{
		Datasource:  "platform_core",
		Datasources: []DatasourceConfig{{Name: "platform_billing"}, {Name: "analytics_events"}},
	})
	for _, name := range []string{"platform_core", "platform_billing", "analytics_events"} {
		if _, err := pools.For(name); err != nil {
			t.Fatalf("For(%s): %v", name, err)
		}
	}
	core, _ := pools.For("platform_core")
	events, _ := pools.For("analytics_events")
	pools.Close()
	if !core.physical.isClosed() || !events.physical.isClosed() {
		t.Error("Pools.Close must release every handle so each physical pool closes")
	}
	if core.physical.refs != 0 || events.physical.refs != 0 {
		t.Errorf("refs after Close = %d/%d, want 0/0", core.physical.refs, events.physical.refs)
	}
}

// TestPools_ClosedDatasourceFailsWithAnErrorNotAPanic proves a closed logical
// pool over a still-open shared physical pool fails every query surface with a
// db.connection error — Exec, Query, QueryRow's Scan, WithTx's begin,
// Querier(ctx).Exec, Acquire, Ping, and the pgx view's own methods — and never
// panics or borrows a connection its sibling still holds, while the sibling
// keeps its handle and its search_path. The seam opens no live pgxpool, so the
// sibling is asserted on its handle and acquire request rather than on a query.
func TestPools_ClosedDatasourceFailsWithAnErrorNotAPanic(t *testing.T) {
	spectest.Proves(t, "go/shared-datasource-pooling", "shared-pool-lifetime", "a-closed-datasource-fails-with-an-error-not-a-panic")
	shared := newPhysicalPool(nil, PoolConfig{}.withDefaults(), "platform_core")
	core := newLogicalPool(shared, PoolConfig{SearchPath: "core"})
	if !shared.retain() {
		t.Fatal("precondition: the shared physical entry accepts a second handle")
	}
	billing := newLogicalPool(shared, PoolConfig{SearchPath: "billing"})
	ctx := context.Background()

	core.Close()

	wantErr := func(path string, err error) {
		t.Helper()
		if err == nil || !perrors.Is(err, CodeConnection) {
			t.Errorf("%s on a closed datasource = %v, want a %s error", path, err, CodeConnection)
		}
	}
	if core.PGXPool() != nil {
		t.Error("PGXPool() on a closed datasource must be nil")
	}
	_, err := core.Exec(ctx, "SELECT 1")
	wantErr("Exec", err)
	_, err = core.Query(ctx, "SELECT 1")
	wantErr("Query", err)
	var one int
	wantErr("QueryRow(...).Scan", core.QueryRow(ctx, "SELECT 1").Scan(&one))
	wantErr("WithTx", WithTx(ctx, core, func(context.Context) error {
		t.Error("WithTx must not run the callback when begin fails")
		return nil
	}))
	_, err = core.Querier(ctx).Exec(ctx, "SELECT 1")
	wantErr("Querier(ctx).Exec", err)
	_, err = core.Acquire(ctx)
	wantErr("Acquire", err)
	wantErr("Ping", core.Ping(ctx))
	if core.Stats() != nil || core.PoolUtilization() != (PoolUtilization{}) {
		t.Error("Stats/PoolUtilization on a closed datasource must be nil/zero")
	}

	// The nil view itself is safe on every method, the shape pgx callers hold.
	var view *PGXPool
	_, err = view.Acquire(ctx)
	wantErr("view.Acquire", err)
	wantErr("view.AcquireFunc", view.AcquireFunc(ctx, func(*pgxpool.Conn) error { return nil }))
	_, err = view.Begin(ctx)
	wantErr("view.Begin", err)
	_, err = view.BeginTx(ctx, pgx.TxOptions{})
	wantErr("view.BeginTx", err)
	_, err = view.CopyFrom(ctx, pgx.Identifier{"t"}, []string{"v"}, pgx.CopyFromRows(nil))
	wantErr("view.CopyFrom", err)
	results := view.SendBatch(ctx, &pgx.Batch{})
	_, err = results.Exec()
	wantErr("view.SendBatch().Exec", err)
	_, err = results.Query()
	wantErr("view.SendBatch().Query", err)
	wantErr("view.SendBatch().QueryRow().Scan", results.QueryRow().Scan(&one))
	if err := results.Close(); err != nil {
		t.Errorf("view.SendBatch().Close = %v, want nil", err)
	}
	if view.Stat() != nil || view.Config() != nil {
		t.Error("view.Stat/Config on a closed datasource must be nil")
	}

	// The sibling still holds its handle on the open shared entry and marks
	// its acquires with its own search_path.
	if shared.isClosed() {
		t.Fatal("closing platform_core must leave the shared physical pool open for platform_billing")
	}
	if got, ok := searchPathRequestFrom(billing.acquireContext(ctx)); !ok || got != "billing" {
		t.Errorf("platform_billing acquire request = (%q, %v), want (billing, true)", got, ok)
	}
	billing.Close()
	if !shared.isClosed() {
		t.Error("closing the last datasource must close the shared physical pool")
	}
}
