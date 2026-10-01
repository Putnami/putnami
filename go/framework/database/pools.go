package database

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	perrors "go.putnami.dev/errors"
	pdb "go.putnami.dev/protocol/database"
	protocolmigration "go.putnami.dev/protocol/migration"
)

// Pools is the runtime registry of datasource-bound connection pools a workload
// owns. The single-pool surface (database.NewPlugin with one Datasource) reaches
// exactly one pool; a workload that declares several datasources — several owned
// schemas on one physical database, or several physical databases — resolves a
// *Pools from the DI container and asks it for the pool bound to a named
// datasource:
//
//	pools := /* injected *database.Pools */
//	iam, err := pools.For("platform_iam") // own search_path over the database's shared pool
//
// Each datasource's pool is opened lazily on first For (or Default) and cached,
// so a process that touches only a subset of the declared datasources pays the
// connection cost of only those it uses. A named datasource's physical
// connection and search_path are resolved from the deployer-injected
// DATABASE_BINDINGS (see resolveBinding) exactly like the single-pool plugin
// when a binding is present; without one, the per-datasource PoolConfig can
// supply a local DSN or structured Connection fallback.
//
// Datasources are logical; physical pools are per database. A datasource is
// one schema, so a workload that owns several schemas of one physical database
// declares several datasources whose effective connections are identical.
// The registry keys the physical *pgxpool.Pool by that connection identity —
// the rendered DSN of the effective PoolConfig — never by datasource name, so
// those datasources share ONE physical pool (one set of idle backends, one
// ceiling) and differ only in the search_path each acquire sets (see
// search_path.go and doc/adr/0003). Two datasources of one database must
// declare the same tuning and identity hooks: a shared pool takes one, and a
// disagreement is a declaration error, never a silently larger ceiling or a
// silent second pool. Each datasource's identity is computed from its own
// config alone, so the sharing is deterministic whatever the open order.
//
// Pools is safe for concurrent use.
type Pools struct {
	// newPhysical opens the *pgxpool.Pool for a resolved PoolConfig (defaults
	// applied). It is a seam the plugin wires to connectPhysical and tests
	// override to count physical opens without a database.
	newPhysical func(ctx context.Context, cfg PoolConfig) (*pgxpool.Pool, error)

	// configBinding returns the managed binding document carried by the
	// "database" config section (nil when the section carries none) plus a
	// diagnostic explaining a nil document (see managedBindingDiag), so a
	// nil-binding open names the exact gap. It resolves the document lazily and
	// memoized, before or after Configure runs. A document from config wins over
	// the DATABASE_BINDINGS env transport; a malformed section is an error, failing
	// the open loudly instead of silently degrading to env or local fallbacks.
	configBinding func() (*pdb.Binding, string, error)

	// specs is immutable after construction: datasource key -> declaration.
	specs map[string]datasourceSpec
	// defaultKey is the key of the default datasource (the plugin's primary),
	// or "" when the workload declares only named datasources.
	defaultKey string

	// mu guards cache, loading, physical and connecting. It is held only for
	// fast bookkeeping, never across a pool open (a real connect, bounded by
	// ConnectTimeout): the physical open in acquirePhysical runs with mu
	// released, so opening one datasource never blocks resolving,
	// health-checking, or closing another, and shutdown does not stall behind
	// an in-flight cold connect.
	mu sync.Mutex
	// cache holds the opened logical pools: datasource key -> pool.
	cache map[string]*Pool
	// loading dedups concurrent first opens of the same datasource: key ->
	// in-flight open. It mirrors the DI container's build-state pattern.
	loading map[string]*poolLoad
	// physical holds the opened physical pools: connection identity (the
	// rendered effective DSN — it carries the password, so it is never logged
	// or put in an error) -> shared pool.
	physical map[string]*physicalPool
	// connecting dedups concurrent first opens of the same physical identity by
	// two datasources: identity -> in-flight connect.
	connecting map[string]*physicalLoad
}

// poolLoad is one in-flight pool open. The goroutine that registers it runs the
// open; concurrent callers for the same datasource wait on done and observe the
// same result. A failed open is not cached, so a later call retries.
type poolLoad struct {
	done chan struct{}
	pool *Pool
	err  error
}

// physicalLoad is one in-flight physical connect. The datasource that registers
// it connects; another datasource resolving to the same identity meanwhile waits
// on done, then finds the pool in the cache (or, on failure, the raw error to
// label with its own name — a failed connect is not cached, so a later call
// retries).
type physicalLoad struct {
	done chan struct{}
	err  error
}

// datasourceSpec is one declared datasource: the logical name used to resolve a
// binding (empty for the legacy explicit-connection default) plus the PoolConfig
// carrying tuning/identity (and, for the legacy default, the explicit
// connection).
type datasourceSpec struct {
	name string
	pool PoolConfig
}

// newPools builds a registry over the given specs. It opens nothing — pools are
// materialized lazily by For/Default. configBinding supplies the managed
// binding document resolved from the "database" config section (may be nil,
// meaning env-transport/local fallbacks only).
func newPools(specs map[string]datasourceSpec, defaultKey string, newPhysical func(ctx context.Context, cfg PoolConfig) (*pgxpool.Pool, error), configBinding func() (*pdb.Binding, string, error)) *Pools {
	return &Pools{
		newPhysical:   newPhysical,
		configBinding: configBinding,
		specs:         specs,
		defaultKey:    defaultKey,
		cache:         make(map[string]*Pool, len(specs)),
		loading:       make(map[string]*poolLoad),
		physical:      make(map[string]*physicalPool),
		connecting:    make(map[string]*physicalLoad),
	}
}

// For returns the pool bound to the named datasource, opening it on first use
// and caching it for the registry's lifetime. It fails with a clear
// db.datasource error when the name was not declared (listing the datasources
// the workload does declare), and propagates the binding/connection error when
// the datasource is declared but its pool cannot be resolved or opened.
func (ps *Pools) For(name string) (*Pool, error) {
	key := strings.TrimSpace(name)
	if key == "" {
		return nil, perrors.Newf(CodeDatasource, "a datasource name is required to resolve a pool")
	}
	return ps.open(key)
}

// Default returns the pool for the workload's default (primary) datasource —
// the same instance the unnamed *Pool token resolves to. It fails when the
// workload declares only named datasources, pointing the caller at For.
func (ps *Pools) Default() (*Pool, error) {
	if ps.defaultKey == "" {
		return nil, perrors.Newf(CodeDatasource,
			"no default datasource: this workload declares only named datasources (%s); resolve one with Pools.For",
			ps.declaredNames())
	}
	return ps.open(ps.defaultKey)
}

// open returns the pool bound to key, opening it at most once across concurrent
// callers and caching the result. The physical open (binding resolution plus
// connect) runs in openPool with ps.mu released, so a slow open of one
// datasource never blocks resolving, health-checking, or closing another. A
// failed open is not cached: every in-flight caller observes the same error and
// the next call retries.
func (ps *Pools) open(key string) (*Pool, error) {
	ps.mu.Lock()
	if pool, ok := ps.cache[key]; ok {
		ps.mu.Unlock()
		return pool, nil
	}
	spec, ok := ps.specs[key]
	if !ok {
		ps.mu.Unlock()
		return nil, perrors.Newf(CodeDatasource,
			"unknown datasource %q; this workload declares: %s", key, ps.declaredNames())
	}
	if load, ok := ps.loading[key]; ok {
		// A first open is already in flight for this datasource; share its
		// outcome instead of opening a second connection.
		ps.mu.Unlock()
		<-load.done
		return load.pool, load.err
	}
	load := &poolLoad{done: make(chan struct{})}
	ps.loading[key] = load
	ps.mu.Unlock()

	pool, err := ps.openPool(key, spec)

	ps.mu.Lock()
	delete(ps.loading, key)
	if err == nil {
		ps.cache[key] = pool
	}
	load.pool, load.err = pool, err
	close(load.done)
	ps.mu.Unlock()
	return pool, err
}

// openPool resolves key's binding into its effective PoolConfig, then binds a
// logical pool to the physical pool of that config's connection identity —
// opening the physical pool when this datasource is the first to reach it,
// sharing it otherwise. It holds no lock across the connect, so a slow connect
// cannot block the rest of the registry. key labels the binding-resolution,
// tuning, and connection errors.
func (ps *Pools) openPool(key string, spec datasourceSpec) (*Pool, error) {
	binding, diag, err := ps.injectedBinding()
	if err != nil {
		return nil, perrors.Wrapf(err, CodeBinding, "resolve datasource binding", perrors.String("datasource", key))
	}
	cfg, err := effectivePoolConfigFor(spec.name, spec.pool, binding, diag)
	if err != nil {
		return nil, perrors.Wrapf(err, CodeBinding, "resolve datasource binding", perrors.String("datasource", key))
	}
	cfg = cfg.withDefaults()
	identity, err := cfg.resolveDSN()
	if err != nil {
		return nil, perrors.Wrapf(err, CodeConnection, "open pool for datasource", perrors.String("datasource", key))
	}
	timeout := cfg.ConnectTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	pp, err := ps.acquirePhysical(ctx, key, identity, cfg)
	if err != nil {
		return nil, err
	}
	return newLogicalPool(pp, cfg), nil
}

// acquirePhysical returns the physical pool for identity with one reference
// taken for the caller, connecting it when no datasource has yet. A pool already
// opened by another datasource is shared only when cfg's tuning agrees with the
// tuning it was created with; a disagreement is a declaration error naming both
// datasources and each differing field. Concurrent first opens of one identity
// collapse to one connect: the second datasource waits on the first's
// physicalLoad, then re-enters to find the pool (and have its tuning checked)
// or, when the connect failed, to label the shared error with its own name.
// The connect runs with ps.mu released.
func (ps *Pools) acquirePhysical(ctx context.Context, key, identity string, cfg PoolConfig) (*physicalPool, error) {
	tuning := tuningOf(cfg)
	for {
		ps.mu.Lock()
		if pp, ok := ps.physical[identity]; ok {
			if diffs := pp.tuning.diff(tuning); len(diffs) > 0 {
				ps.mu.Unlock()
				return nil, tuningMismatchError(pp.opener, key, diffs)
			}
			if pp.retain() {
				ps.mu.Unlock()
				return pp, nil
			}
			// Every handle was released and the physical pool closed underneath
			// the cache (a caller closed its logical pools directly); the entry
			// is stale, so drop it and connect anew below.
			delete(ps.physical, identity)
		}
		if load, ok := ps.connecting[identity]; ok {
			ps.mu.Unlock()
			<-load.done
			if load.err != nil {
				return nil, perrors.Wrapf(load.err, CodeConnection, "open pool for datasource", perrors.String("datasource", key))
			}
			continue
		}
		load := &physicalLoad{done: make(chan struct{})}
		ps.connecting[identity] = load
		ps.mu.Unlock()

		pool, err := ps.newPhysical(ctx, cfg)

		ps.mu.Lock()
		delete(ps.connecting, identity)
		var pp *physicalPool
		if err == nil {
			pp = newPhysicalPool(pool, cfg, key)
			ps.physical[identity] = pp
		}
		load.err = err
		close(load.done)
		ps.mu.Unlock()
		if err != nil {
			return nil, perrors.Wrapf(err, CodeConnection, "open pool for datasource", perrors.String("datasource", key))
		}
		return pp, nil
	}
}

// PhysicalCount reports the number of distinct physical pools currently open
// through the registry — the number of databases the opened datasources reach,
// not the number of datasources. It is the observable a test uses to prove
// sharing without a database.
func (ps *Pools) PhysicalCount() int {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	n := 0
	for _, pp := range ps.physical {
		if !pp.isClosed() {
			n++
		}
	}
	return n
}

// injectedBinding returns the managed binding document resolved from the config
// section plus the diagnostic for a nil document, tolerating a registry
// constructed without the seam (tests).
func (ps *Pools) injectedBinding() (*pdb.Binding, string, error) {
	if ps.configBinding == nil {
		return nil, "", nil
	}
	return ps.configBinding()
}

// Names returns the datasource names this registry can resolve, sorted.
func (ps *Pools) Names() []string {
	names := make([]string, 0, len(ps.specs))
	for k := range ps.specs {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// HealthCheck pings the default datasource's pool when one is configured,
// opening it on first probe just as the single-pool plugin always has. With no
// default datasource (a named-only workload) it pings every pool already opened
// via For, so the probe reflects the datasources actually in use without
// force-opening idle ones.
func (ps *Pools) HealthCheck(ctx context.Context) error {
	if ps.defaultKey != "" {
		pool, err := ps.Default()
		if err != nil {
			return err
		}
		return pool.Ping(ctx)
	}
	ps.mu.Lock()
	opened := make([]*Pool, 0, len(ps.cache))
	for _, pool := range ps.cache {
		opened = append(opened, pool)
	}
	ps.mu.Unlock()
	for _, pool := range opened {
		if err := pool.Ping(ctx); err != nil {
			return err
		}
	}
	return nil
}

// Close closes every logical pool opened through the registry and drops the
// logical and physical caches, so a second call (e.g. plugin Stop after the
// container's close hook) is a no-op and a later For re-opens. Each logical
// close releases its handle on its physical pool, and the physical pool closes
// with the last handle. It snapshots the open pools under the lock and closes
// them outside it, so closing never stalls behind an in-flight open of another
// datasource.
func (ps *Pools) Close() {
	ps.mu.Lock()
	opened := ps.cache
	ps.cache = make(map[string]*Pool)
	ps.physical = make(map[string]*physicalPool)
	ps.mu.Unlock()
	for _, pool := range opened {
		pool.Close()
	}
}

// declaredNames renders the declared datasource keys as a sorted, human-readable
// list for diagnostics. It reads only the immutable specs map, so it needs no
// lock.
func (ps *Pools) declaredNames() string {
	if len(ps.specs) == 0 {
		return "(none)"
	}
	names := make([]string, 0, len(ps.specs))
	for k := range ps.specs {
		names = append(names, k)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// poolSpecs projects the plugin config into the registry's declared datasources:
// the primary (legacy single-pool Datasource/Pool) keyed by its name — or the
// canonical "default" datasource when the primary connects via an explicit DSN —
// plus one entry per additional named Datasources declaration. The returned
// defaultKey is the primary's key, or "" when only named datasources are
// declared. A blank or duplicate datasource name is a configuration error.
func (c PluginConfig) poolSpecs() (specs map[string]datasourceSpec, defaultKey string, err error) {
	specs = make(map[string]datasourceSpec, len(c.Datasources)+1)

	if c.hasPrimaryPool() {
		name := strings.TrimSpace(c.Datasource)
		key := name
		if key == "" {
			// An explicit-connection primary has no logical name; key it under
			// the canonical default datasource so Default and For("default")
			// both reach it.
			key = protocolmigration.DefaultDatasource
		}
		specs[key] = datasourceSpec{name: name, pool: c.Pool}
		defaultKey = key
	}

	for _, ds := range c.Datasources {
		name := strings.TrimSpace(ds.Name)
		if name == "" {
			return nil, "", perrors.Newf(CodeDatasource, "a Datasources entry requires a non-empty Name")
		}
		if _, dup := specs[name]; dup {
			return nil, "", perrors.Newf(CodeDatasource, "duplicate datasource %q declared on the database plugin", name)
		}
		specs[name] = datasourceSpec{name: name, pool: ds.Pool}
	}

	return specs, defaultKey, nil
}
