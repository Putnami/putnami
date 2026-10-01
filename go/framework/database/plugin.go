package database

import (
	"context"
	stdsql "database/sql"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.putnami.dev/app"
	perrors "go.putnami.dev/errors"
	"go.putnami.dev/inject"
	"go.putnami.dev/logger"
	"go.putnami.dev/migration"
	protocaps "go.putnami.dev/protocol/capabilities"
	pdb "go.putnami.dev/protocol/database"
)

// PluginConfig configures the database plugin.
//
// Datasource names the logical database this plugin connects: it is the
// canonical, driver-neutral surface (database.NewPlugin(PluginConfig{Datasource:
// "auth"})). When a deployer injects DATABASE_BINDINGS (see EnvBinding /
// resolveBinding), the physical connection and owning schema/search_path come
// from that binding, so the same binary reaches staging and prod without
// changing its declared datasource. When no binding is injected, Pool may supply
// a local DSN or structured Connection fallback for dev/CI while keeping the
// same logical datasource name.
//
// Pool is the explicit-connection escape hatch used when no Datasource is named:
// a raw DSN or structured Connection for local/dev or special datasources. It
// remains the internal connection builder the resolved binding feeds into.
//
// Migration carries plugin-level toggles (AutoApply, LockTimeout); the
// per-Migrator fields on MigrationConfig (Datasource, Definitions) are
// ignored here.
//
// Datasources declares additional datasource-bound pools beyond the primary
// Datasource/Pool. A workload that owns several schemas (on one physical
// database, or several) gives each its own logical pool — its own search_path
// — and resolves them by name from the injected *Pools registry
// (pools.For("platform_iam")). Datasources resolving to one physical database
// share one physical pool and must declare the same tuning and identity hooks
// (see Pools). The primary remains the unnamed *Pool token.
type PluginConfig struct {
	Datasource  string
	Pool        PoolConfig
	Datasources []DatasourceConfig
	Migration   *MigrationConfig

	// UnitOfWork opts the workload into a DI request-scoped UnitOfWork: when
	// true the plugin registers a Scoped *UnitOfWork provider, and every
	// repository query on a pool within a request scope transparently joins one
	// transaction per participating datasource, committed on handler success and
	// rolled back on error/panic at the request-scope boundary (see UnitOfWork).
	// It is off by default so existing single-query/autocommit workloads keep
	// their behavior unchanged; opting in makes each request that touches a
	// repository transactional.
	UnitOfWork bool

	// UnitOfWorkTimeout bounds each per-datasource commit/rollback the
	// request-scoped UnitOfWork performs at the boundary, so a wedged
	// commit/rollback cannot pin a pooled connection indefinitely. Zero (the
	// default) disables the bound. Ignored unless UnitOfWork is true.
	UnitOfWorkTimeout time.Duration
}

// DatasourceConfig declares one additional named datasource-bound pool on the
// database plugin (PluginConfig.Datasources). Name is the logical datasource
// resolved from the deployer-injected DATABASE_BINDINGS when present, exactly
// like the primary Datasource. When no binding is injected, Pool may supply this
// datasource's local DSN or structured Connection fallback. Resolve the pool by
// Name from the *Pools registry.
type DatasourceConfig struct {
	Name string
	Pool PoolConfig
}

// hasPrimaryPool reports whether the plugin provides an unnamed *Pool (the
// primary/default datasource). Legacy single-pool usage (no Datasources) always
// does, preserving the existing surface even for an empty or explicit-connection
// config. A multi-pool config provides the unnamed *Pool only when a primary is
// actually configured — a named primary Datasource or an explicit Pool
// connection — so a Datasources-only workload does not fail resolving an
// unconfigured default.
func (c PluginConfig) hasPrimaryPool() bool {
	if len(c.Datasources) == 0 {
		return true
	}
	return strings.TrimSpace(c.Datasource) != "" || c.Pool.hasConnection()
}

// namedDatasources returns every logical datasource name the plugin declares —
// the primary Datasource when named, plus each Datasources entry — for the
// build-time infra requirements. The legacy explicit-connection primary (no
// Datasource name) contributes nothing here; its infra comes from the Pool path.
func (c PluginConfig) namedDatasources() []string {
	var names []string
	if ds := strings.TrimSpace(c.Datasource); ds != "" {
		names = append(names, ds)
	}
	for _, d := range c.Datasources {
		if n := strings.TrimSpace(d.Name); n != "" {
			names = append(names, n)
		}
	}
	return names
}

// effectivePoolConfig returns the PoolConfig the primary pool is built from,
// resolving the plugin's Datasource against the injected managed binding (the
// resolved config section first, DATABASE_BINDINGS as fallback). It resolves
// the config-carried document itself (lazily, memoized) so it does not depend
// on Configure having run.
func (p *Plugin) effectivePoolConfig() (PoolConfig, error) {
	binding, err := p.resolveConfigBinding(context.Background())
	if err != nil {
		return PoolConfig{}, err
	}
	return effectivePoolConfigFor(p.cfg.Datasource, p.cfg.Pool, binding, p.configDiag)
}

// effectivePoolConfigFor returns the PoolConfig a datasource's pool is built
// from. When a logical datasource is named and a managed binding is injected —
// the document carried by the resolved "database" config section (configured,
// resolved once and memoized; see resolveConfigBinding) winning over
// DATABASE_BINDINGS — its physical
// connection and search_path are resolved from the binding and layered over
// the supplied tuning knobs. When no binding is injected, a local base
// connection (DSN/Connection) is used, with a structured Connection defaulting
// to the logical datasource name as its database target. With no datasource
// name, the explicit base connection is always used as-is. It is the shared
// resolver behind both the single-pool plugin and the *Pools registry.
//
// sectionDiag describes why the config section carried no managed binding (see
// managedBindingDiag); it is folded into the no-binding-no-fallback failure so
// the crash names the exact gap — an empty managed overlay reads differently
// from an unmanaged workload — instead of a generic "requires a database
// binding". It is used only on that failure path and ignored otherwise.
func effectivePoolConfigFor(datasource string, base PoolConfig, configured *pdb.Binding, sectionDiag string) (PoolConfig, error) {
	ds := strings.TrimSpace(datasource)
	if ds == "" {
		return base, nil
	}
	resolved, ok, err := resolveDatasourceBinding(configured, ds)
	if err != nil {
		return PoolConfig{}, err
	}
	if !ok {
		if base.hasConnection() {
			if base.Connection.isSet() && base.databaseName() == "" {
				base.Database = ds
			}
			return base, nil
		}
		// Reaching here means neither transport resolved: the config section
		// carried no managed binding (sectionDiag says which shape) and — since
		// resolveDatasourceBinding returned ok=false with no error —
		// DATABASE_BINDINGS is unset. Name both so the failure is
		// self-diagnosing.
		detail := sectionDiag
		if detail == "" {
			detail = "no managed database binding was resolved"
		}
		return PoolConfig{}, perrors.Newf(CodeBinding,
			"datasource %q requires a database binding or local Pool DSN/Connection, but none was resolved: %s and %s is unset; inject the %q config section, set %s, or configure PluginConfig.Pool", ds, detail, EnvBinding, ConfigSection, EnvBinding)
	}
	// Keep the pool's tuning/identity knobs; take the connection and
	// search_path from the resolved binding, clearing any stale build-time
	// connection declaration so the binding is authoritative at runtime.
	base.DSN = resolved.DSN
	base.Connection = resolved.Connection
	base.Database = resolved.Database
	base.Datasource = Datasource{}
	base.Schemas = nil
	base.SearchPath = resolved.SearchPath
	return base, nil
}

type pluginPoolResolver func(owner *app.Module, cached *Pool) (*Pool, error)

// Plugin integrates PostgreSQL into the application lifecycle:
//
//   - Provides a *Pool into the DI container.
//
//   - During Configure, constructs an SQLRunner and registers it into the
//     per-app *migration.Registry. The runner consumes every SQLSource
//     contributed by feature plugins via app.MigrationContributor.
//
//   - The framework's automatic Migrate phase then drives the runner.
//     AutoApply (default false) gates that automatic application; the
//     migrate CLI overrides via ApplyOpts.Force.
//
//     app.New("my-service").
//     Use(database.NewPlugin(database.PluginConfig{
//     Pool: database.PoolConfig{DSN: "postgres://..."},
//     Migration: &database.MigrationConfig{AutoApply: true},
//     }))
type Plugin struct {
	cfg          PluginConfig
	pool         *Pool
	pools        *Pools
	stdDB        *stdsql.DB
	log          *logger.Logger
	owner        *app.Module
	resolvePool  pluginPoolResolver
	openStdlibDB func(pool *Pool) *stdsql.DB
	// newPhysical opens the physical *pgxpool.Pool for a datasource's effective
	// PoolConfig; the registry shares one physical pool across every datasource
	// of the same connection identity (see Pools). Tests override it to count
	// physical opens without a database.
	newPhysical func(ctx context.Context, cfg PoolConfig) (*pgxpool.Pool, error)

	// configBinding is the managed binding document resolved from the
	// "database" config section (nil when the section carries none). It is
	// resolved once, lazily, by whichever of the eager pool open or Configure
	// runs first — the application's eager DI build opens pools BEFORE
	// Configure — and memoized via configOnce so both observe the same
	// document for one container build. Provides resets the memo before each
	// lifecycle pass so Validate/failed Start retries re-read config instead of
	// keeping stale nil/error outcomes. It is preferred over the
	// DATABASE_BINDINGS env transport. Read it through resolveConfigBinding,
	// never directly.
	configBinding *pdb.Binding
	// configDiag explains, for the (nil, nil) resolution outcome, why the
	// config section carried no managed binding (see managedBindingDiag). It is
	// memoized alongside configBinding so a nil-binding pool open — on any
	// datasource, resolved eagerly or lazily — surfaces the same self-diagnosing
	// reason without re-reading config.
	configDiag string
	configErr  error
	configOnce sync.Once
}

// NewPlugin creates a new database plugin.
func NewPlugin(cfg PluginConfig) *Plugin {
	return &Plugin{
		cfg:         cfg,
		log:         logger.Default().Named("database.plugin"),
		newPhysical: connectPhysical,
		resolvePool: func(owner *app.Module, cached *Pool) (*Pool, error) {
			if cached != nil {
				return cached, nil
			}
			if owner == nil || owner.Container() == nil {
				return nil, perrors.Newf(CodeMigrationStartup, "sql migration startup requires a container-managed pool")
			}
			value, err := owner.Container().Get(inject.TokenOf[*Pool]())
			if err != nil {
				return nil, perrors.Wrapf(err, CodeMigrationStartup, "resolve sql pool for migrations")
			}
			pool, ok := value.(*Pool)
			if !ok {
				return nil, perrors.Newf(CodeMigrationStartup, "resolve sql pool for migrations: unexpected type %T", value)
			}
			return pool, nil
		},
		// The runner's database/sql handle acquires every connection under
		// the pool's datasource search_path (see newStdlibDB), so a
		// schema-less bundle lands in that datasource's schema even though the
		// physical pool may be shared with other datasources.
		openStdlibDB: newStdlibDB,
	}
}

// Name returns the plugin name.
func (p *Plugin) Name() string { return "database" }

// DesignInfraRequirements projects the same logical datasource names used by
// runtime binding and the infra sidecar through app's bounded design seam.
func (p *Plugin) DesignInfraRequirements() []app.DesignInfraRequirement {
	names := p.cfg.namedDatasources()
	if len(names) == 0 {
		for _, declaration := range deriveDatabaseDecls(p.cfg.Pool) {
			names = append(names, declaration.name)
		}
	}
	sort.Strings(names)
	requirements := make([]app.DesignInfraRequirement, 0, len(names))
	for _, name := range names {
		if len(requirements) > 0 && requirements[len(requirements)-1].Name == name {
			continue
		}
		requirements = append(requirements, app.DesignInfraRequirement{Name: name, Kind: protocaps.InfraKindDatabase})
	}
	return requirements
}

// Provides registers the datasource-bound connection pools as DI providers:
//
//   - *Pools, the registry of every datasource the plugin declares (the
//     primary plus each PluginConfig.Datasources entry), each pool opened
//     lazily on first use and resolvable by name via Pools.For.
//
//   - *Pool, the primary/default datasource — kept as the unnamed token so
//     existing single-datasource consumers, the migration runner, and the
//     health probe resolve it unchanged. It delegates to the registry's
//     default, so the unnamed *Pool and Pools.Default are the same instance.
//     It is omitted only for a Datasources-only workload with no primary.
//
// The registry owns pool lifetimes; its close hook (and Stop) close every pool
// that was opened.
func (p *Plugin) Provides() []inject.Registration {
	p.resetConfigBinding()
	regs := []inject.Registration{
		inject.Provide(
			inject.TokenOf[*Pools](),
			func(_ inject.Resolver) (any, error) {
				return p.buildPools()
			},
			inject.WithOnClose(func() error {
				if p.pools != nil {
					p.pools.Close()
				}
				return nil
			}),
		),
	}
	if p.cfg.hasPrimaryPool() {
		regs = append(regs, inject.Provide(
			inject.TokenOf[*Pool](),
			func(r inject.Resolver) (any, error) {
				pools, err := inject.ResolveAs[*Pools](r, inject.TokenOf[*Pools]())
				if err != nil {
					return nil, perrors.Wrapf(err, CodeConnection, "resolve database pools")
				}
				pool, err := pools.Default()
				if err != nil {
					return nil, err
				}
				p.pool = pool
				return pool, nil
			},
			inject.WithDeps(inject.TokenOf[*Pools]()),
		))
	}
	if p.cfg.UnitOfWork {
		// A fresh UnitOfWork per request scope. It depends on nothing (pools are
		// handed to it at enroll time via the query path), so the factory is
		// cheap and opens no connection until a repository query enrolls a pool.
		// It implements inject.ScopeFinalizer, so the request-scope boundary
		// commits it on success and rolls it back on error/panic.
		timeout := p.cfg.UnitOfWorkTimeout
		regs = append(regs, inject.Provide(
			inject.TokenOf[*UnitOfWork](),
			func(_ inject.Resolver) (any, error) {
				return newUnitOfWork(timeout), nil
			},
			inject.WithScope(inject.Scoped),
		))
	}
	return regs
}

// buildPools constructs (once) the registry of datasource-bound pools from the
// plugin config. It resolves no bindings and opens no connections — those are
// deferred to Pools.For/Default — so it is cheap and surfaces only config
// errors (a blank or duplicate datasource name). The registry reads the
// config-sourced binding through the memoized resolveConfigBinding closure, so
// the open path sees the same document before and after Configure.
func (p *Plugin) buildPools() (*Pools, error) {
	if p.pools != nil {
		return p.pools, nil
	}
	specs, defaultKey, err := p.cfg.poolSpecs()
	if err != nil {
		return nil, err
	}
	p.pools = newPools(specs, defaultKey, p.newPhysical, func() (*pdb.Binding, string, error) {
		b, err := p.resolveConfigBinding(context.Background())
		// resolveConfigBinding completes configOnce before returning, so
		// configDiag is populated for the (nil, nil) outcome the registry needs
		// to explain a nil-binding open.
		return b, p.configDiag, err
	})
	return p.pools, nil
}

// Configure resolves the managed binding document from the "database" config
// section (see applyResolvedConfig) and registers the SQLRunner into the
// per-app *migration.Registry. It does not apply migrations — that is the
// framework's Migrate phase (which respects AutoApply) and the migrate CLI
// (which forces).
//
// Configure also captures the owning module so CheckHealth (called on
// the health endpoint request path) can resolve the *Pool lazily.
func (p *Plugin) Configure(ctx context.Context, owner *app.Module) error {
	p.owner = owner
	if err := p.applyResolvedConfig(ctx); err != nil {
		return err
	}
	if p.cfg.Migration == nil {
		return nil
	}
	if owner == nil || owner.Container() == nil {
		return perrors.Newf(CodeMigrationStartup, "database plugin requires a DI container")
	}

	value, err := owner.Container().Get(inject.TokenOf[*migration.Registry]())
	if err != nil {
		return perrors.Wrapf(err, CodeMigrationStartup, "resolve migration registry")
	}
	registry, ok := value.(*migration.Registry)
	if !ok {
		return perrors.Newf(CodeMigrationStartup,
			"resolve migration registry: unexpected type %T", value)
	}

	runner := newSQLRunner(SQLRunnerOptions{
		Registry:    registry,
		OpenDB:      p.openDBForRunner(owner),
		LockTimeout: p.cfg.Migration.LockTimeout,
		AutoApply:   p.cfg.Migration.AutoApply,
	})
	if err := registry.RegisterRunner(runner); err != nil {
		return perrors.Wrap(err, CodeMigrationStartup)
	}
	return nil
}

// applyResolvedConfig resolves the managed binding document carried by the
// "database" section of the resolved application config — the
// config-resolution transport for managed bindings — surfacing a malformed
// section at startup. The eager pool opens may already have resolved it (the
// DI build runs before Configure); resolveConfigBinding memoizes so both
// paths observe the same document. A document there wins over
// DATABASE_BINDINGS; with none, the env transport and the local Pool
// fallbacks behave exactly as before, so code-only local serve/test flows are
// unchanged.
func (p *Plugin) applyResolvedConfig(ctx context.Context) error {
	_, err := p.resolveConfigBinding(ctx)
	return err
}

// resolveConfigBinding resolves (once) the managed binding document carried
// by the "database" config section and memoizes the outcome, so the eager
// pool-open path and Configure — whichever runs first — observe the same
// document. It holds no lock beyond the sync.Once (the pool registry calls it
// with its mutex released), so there is no lock-ordering hazard.
func (p *Plugin) resolveConfigBinding(ctx context.Context) (*pdb.Binding, error) {
	p.configOnce.Do(func() {
		p.configBinding, p.configDiag, p.configErr = loadConfigBindingWithDiag(ctx)
	})
	return p.configBinding, p.configErr
}

// resetConfigBinding clears the per-container-build config memo before the app
// collects provider registrations for a lifecycle pass. This preserves the
// eager-build/Configure ordering guarantee within one pass while allowing a
// later Validate/Start retry to observe newly available or repaired config.
func (p *Plugin) resetConfigBinding() {
	p.configBinding = nil
	p.configDiag = ""
	p.configErr = nil
	p.configOnce = sync.Once{}
}

// openDBForRunner returns a thunk the SQLRunner calls to obtain a
// *sql.DB at first use. It resolves the pool lazily so plugin
// construction order doesn't matter.
func (p *Plugin) openDBForRunner(owner *app.Module) func() (*stdsql.DB, error) {
	return func() (*stdsql.DB, error) {
		pool, err := p.resolvePool(owner, p.pool)
		if err != nil {
			return nil, err
		}
		if pool == nil {
			return nil, perrors.Newf(CodeMigrationStartup, "sql migration requires an initialized pool")
		}
		p.pool = pool
		db := p.openStdlibDB(pool)
		if db == nil {
			return nil, perrors.Newf(CodeMigrationStartup, "open stdlib database for migrations")
		}
		p.stdDB = db
		return db, nil
	}
}

// CheckHealth probes the workload's pools via Ping. Once the primary pool has
// been materialized it pings that cached reference directly; otherwise it
// resolves the *Pools registry through the owning module's container and
// delegates to Pools.HealthCheck, which probes the default datasource (a
// Datasources-only workload has it ping the pools already opened via For).
//
// Lazy materialization is intentional — a probe that can't open the pool
// should surface unhealthy rather than mask a misconfiguration — but it means
// the first /healthz after boot may carry pool construction cost on top of
// Ping. The platform plugin's per-probe timeout (Config.ProbeTimeout) bounds
// that worst case. Subsequent probes reuse the cached pool and run in
// microseconds.
//
// Implements app.HealthChecker: the http.HealthPlugin auto-discovers this
// probe and registers it under the plugin's Name ("database").
func (p *Plugin) CheckHealth(ctx context.Context) error {
	if p.pool != nil {
		return p.pool.Ping(ctx)
	}
	if p.owner == nil || p.owner.Container() == nil {
		return perrors.Newf(CodeConnection, "health: database plugin not yet configured")
	}
	value, err := p.owner.Container().Get(inject.TokenOf[*Pools]())
	if err != nil {
		return perrors.Wrapf(err, CodeConnection, "health: resolve database pools")
	}
	pools, ok := value.(*Pools)
	if !ok {
		return perrors.Newf(CodeConnection, "health: resolve database pools: unexpected type %T", value)
	}
	return pools.HealthCheck(ctx)
}

// Stop closes every pool the plugin opened plus the stdlib DB handle. The
// *Pools registry owns pool lifetimes (the primary/default pool included), so
// closing it covers them; its Close is idempotent with the container's close
// hook. A directly-set p.pool with no registry (test seam) is closed on its own.
func (p *Plugin) Stop(_ context.Context, _ *app.Module) error {
	if p.stdDB != nil {
		_ = p.stdDB.Close() //nolint:errcheck // best-effort close
	}
	switch {
	case p.pools != nil:
		p.pools.Close()
	case p.pool != nil:
		p.pool.Close()
	}
	return nil
}
