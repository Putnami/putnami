package database

import (
	"context"
	stdsql "database/sql"
	"database/sql/driver"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"

	"go.putnami.dev/errors"
)

// physicalPool is one *pgxpool.Pool shared by every logical *Pool whose
// datasource resolves to the same connection identity (the rendered DSN of its
// effective PoolConfig; see Pools). A datasource is one schema, so a workload
// reading many schemas of one database holds one physical pool and one set of
// idle backends instead of one per datasource; what differs between its
// datasources — search_path — is set per acquire (see search_path.go).
//
// Logical pools hold references: Pool.Close releases one handle and the
// pgxpool closes when the last handle is released, so closing one datasource
// never kills another datasource's connections and a standalone NewPool (one
// handle) keeps closing its own pool.
type physicalPool struct {
	pool *pgxpool.Pool
	// tuning is what the pgxpool was created with; every datasource that later
	// resolves to this pool must declare it identically (see poolTuning).
	tuning poolTuning
	// opener is the datasource that created the pool, named in the tuning
	// mismatch diagnostic alongside the datasource that disagrees.
	opener string

	mu     sync.Mutex
	refs   int
	closed bool
}

// newPhysicalPool wraps pool, created for opener with cfg's tuning, holding one
// reference for the caller.
func newPhysicalPool(pool *pgxpool.Pool, cfg PoolConfig, opener string) *physicalPool {
	return &physicalPool{pool: pool, tuning: tuningOf(cfg), opener: opener, refs: 1}
}

// retain takes one more reference. It reports false when the pool has already
// closed (every handle was released), so a registry holding a stale entry
// re-opens instead of handing out a closed pool.
func (pp *physicalPool) retain() bool {
	pp.mu.Lock()
	defer pp.mu.Unlock()
	if pp.closed {
		return false
	}
	pp.refs++
	return true
}

// release drops one reference and closes the pgxpool when it was the last. The
// pgxpool Close blocks until every acquired connection is released, so it runs
// outside pp.mu.
func (pp *physicalPool) release() {
	pp.mu.Lock()
	if pp.closed {
		pp.mu.Unlock()
		return
	}
	pp.refs--
	if pp.refs > 0 {
		pp.mu.Unlock()
		return
	}
	pp.closed = true
	pp.mu.Unlock()
	if pp.pool != nil {
		pp.pool.Close()
	}
}

// isClosed reports whether the last handle was released.
func (pp *physicalPool) isClosed() bool {
	pp.mu.Lock()
	defer pp.mu.Unlock()
	return pp.closed
}

// poolTuning is the subset of PoolConfig a physical pool is CREATED with, and
// that every datasource sharing the pool must therefore declare identically: the
// sizing and lifecycle bounds, the statement_timeout startup parameter, and the
// identity/token hooks the connections authenticate through. ConnectTimeout,
// SlowQueryThreshold, the observers, SearchPath and the declarative infra
// fields (Engine, Datasource, Database, Schemas) are per-logical-pool and stay
// out of it. Hooks are compared by CODE identity (nil-aware), the only
// comparison Go offers for funcs: two closures of the same function literal
// capturing different values compare equal and would share the pool the first
// one resolved. Declare hooks as shared functions — top-level, or one closure
// value reused on every datasource — and give a datasource that needs a
// different principal a different connection identity.
type poolTuning struct {
	maxConns          int32
	minConns          int32
	maxConnLifetime   time.Duration
	maxConnIdleTime   time.Duration
	healthCheckPeriod time.Duration
	statementTimeout  time.Duration
	identityResolver  uintptr
	tokenFetcher      uintptr
}

// tuningOf projects cfg (already carrying defaults) onto the compared subset.
func tuningOf(cfg PoolConfig) poolTuning {
	return poolTuning{
		maxConns:          cfg.MaxConns,
		minConns:          cfg.MinConns,
		maxConnLifetime:   cfg.MaxConnLifetime,
		maxConnIdleTime:   cfg.MaxConnIdleTime,
		healthCheckPeriod: cfg.HealthCheckPeriod,
		statementTimeout:  cfg.StatementTimeout,
		identityResolver:  funcIdentity(cfg.IdentityResolver),
		tokenFetcher:      funcIdentity(cfg.TokenFetcher),
	}
}

// funcIdentity returns the code pointer of f, or 0 for nil, so two PoolConfigs
// pointing at the same top-level function compare equal and nil compares equal
// only to nil. It is code identity, not closure identity: captured values are
// invisible to it (see poolTuning).
func funcIdentity(f func(context.Context) (string, error)) uintptr {
	if f == nil {
		return 0
	}
	return reflect.ValueOf(f).Pointer()
}

// diff lists every field on which other disagrees with t, rendered for the
// declaration error ("MaxConns 10 vs 4"; a hook is named without values since a
// function has no printable one). Empty when the tunings agree.
func (t poolTuning) diff(other poolTuning) []string {
	var diffs []string
	value := func(name string, a, b any) {
		if a != b {
			diffs = append(diffs, fmt.Sprintf("%s %v vs %v", name, a, b))
		}
	}
	value("MaxConns", t.maxConns, other.maxConns)
	value("MinConns", t.minConns, other.minConns)
	value("MaxConnLifetime", t.maxConnLifetime, other.maxConnLifetime)
	value("MaxConnIdleTime", t.maxConnIdleTime, other.maxConnIdleTime)
	value("HealthCheckPeriod", t.healthCheckPeriod, other.healthCheckPeriod)
	value("StatementTimeout", t.statementTimeout, other.statementTimeout)
	if t.identityResolver != other.identityResolver {
		diffs = append(diffs, "IdentityResolver differs")
	}
	if t.tokenFetcher != other.tokenFetcher {
		diffs = append(diffs, "TokenFetcher differs")
	}
	return diffs
}

// tuningMismatchError is the declaration error raised when datasource `other`
// resolves to the physical database `opener` already opened with a different
// tuning. It names both datasources and each differing field and never the
// connection identity, which carries the password.
func tuningMismatchError(opener, other string, diffs []string) error {
	return errors.Newf(CodeDatasource,
		"datasources %q and %q resolve to the same physical database but declare different pool tuning (%s); a shared pool takes one tuning — declare it identically on every datasource of that database",
		opener, other, strings.Join(diffs, ", "))
}

// newStdlibDB returns a database/sql handle over pool's shared physical pool
// whose every connection is acquired under pool's search_path. It replaces
// stdlib.OpenDBFromPool for the migration runner (plugin) and ApplyToPool: the
// connector marks each Connect context with the datasource before delegating
// to pgx's pool connector, and MaxIdleConns is zero (OpenDBFromPool's own rule,
// since the connections belong to the pgxpool), so database/sql re-acquires per
// operation and every runner statement — a schema-less bundle included — lands
// in this datasource's search_path. Closing the *sql.DB releases its
// connections to the pgxpool; it never closes the pool. Nil when pool is not
// open.
func newStdlibDB(pool *Pool) *stdsql.DB {
	if pool == nil || pool.PGXPool() == nil {
		return nil
	}
	db := stdsql.OpenDB(datasourceConnector{inner: stdlib.GetPoolConnector(pool.physical.pool), pool: pool})
	db.SetMaxIdleConns(0)
	return db
}

// datasourceConnector is a driver.Connector over pgx's pool connector that binds
// every connection it opens to one datasource: Connect marks the context with
// the datasource's search_path, so the pgxpool acquire it delegates to prepares
// the connection like any other acquire through the logical pool.
type datasourceConnector struct {
	inner driver.Connector
	pool  *Pool
}

func (c datasourceConnector) Connect(ctx context.Context) (driver.Conn, error) {
	return c.inner.Connect(c.pool.acquireContext(ctx))
}

func (c datasourceConnector) Driver() driver.Driver { return c.inner.Driver() }
