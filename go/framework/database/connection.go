// Package database provides PostgreSQL integration for the Putnami Go
// framework. It wraps pgx/v5 for connection pooling, adds transaction
// management via context.Context, a generic repository pattern, a SQL
// migration runner that plugs into the transversal migration framework
// (go.putnami.dev/migration), and a lightweight query builder.
package database

import (
	"context"
	stderrors "errors"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"go.putnami.dev/errors"
	"go.putnami.dev/logger"
	"go.putnami.dev/protocol/infra"
)

// PoolConfig holds connection pool configuration.
//
// The logical datasource (name + schema) and the physical connection are
// separate concerns, resolved with a fixed precedence:
//
//   - Datasource (code) declares the logical (name, schema). It is what the
//     build emits into infra/requirements.json, so the deployer can provision
//     and migrate against a resolvable datasource regardless of how the
//     runtime connects. The name also supplies the database a built
//     connection targets.
//   - Connection (data plane / env at bootstrap) supplies the physical
//     connection — instance/host/user — while the declared datasource is
//     untouched.
//   - DSN (escape hatch) wins outright: when set, it is the connection and
//     Connection is ignored. The declared Datasource still drives infra.
//
// The same "stated wins" rule governs the pool parameters inside the
// connection. When the DSN (or a structured Connection's Params, which are
// rendered into it) states pool_max_conns, pool_min_conns,
// pool_max_conn_lifetime, pool_max_conn_idle_time or
// pool_health_check_period, that value is kept and the matching struct field is
// ignored; the struct fills only what the connection leaves unsaid. A
// connection that states none of them behaves exactly as if the rule did not
// exist, which is every production binding emitted today.
type PoolConfig struct {
	// DSN is the PostgreSQL connection string. libpq-style or postgres:// URL.
	// It is the connection escape hatch — when set it wins over Connection
	// outright — and stays the right choice for local/dev or special
	// datasources. It does not affect the infra datasource the build emits;
	// that comes from Datasource (or the legacy Database).
	//
	// When the user is empty in a DSN for a /cloudsql/... socket, the native GCP
	// identity resolver supplies the runtime principal. Other hosts require
	// IdentityResolver.
	//
	// When the password is empty for a /cloudsql/... Unix socket, native GCP
	// access tokens supply it per connection. Other Unix sockets require
	// TokenFetcher; on TCP, an empty password is fine when a local proxy handles
	// auth.
	DSN string

	// Datasource declares the pool's logical datasource — the same
	// (Name, Schema) shape SQL migration sources declare (see Datasource and
	// NewSQLSource). Declaring it here is the uniform way a workload names the
	// database it owns: the build emits (Name, Schema) into
	// infra/requirements.json so the deployer resolves a real datasource even
	// when the runtime connects via a raw DSN, and Name supplies the database
	// a Connection-built DSN targets. When set, both Name and Schema are
	// required; the zero value falls back to the legacy Database/Schemas
	// fields. Name matches the migration datasource the pool's sources target,
	// collapsing the former pool-vs-migration "default"/named split into one
	// entry.
	Datasource Datasource

	// Connection supplies the physical connection (instance/host/user) when
	// DSN is empty, decoupled from the declared Datasource so the data plane
	// can override where the workload connects at bootstrap without touching
	// its declared name/schema. Ignored when DSN is set.
	Connection Connection

	// SearchPath is the owning schema set as the session search_path on every
	// connection this pool hands out. It is the schema half of a resolved
	// datasource binding (see PoolConfigFromBinding): a canonical database
	// Binding declares the schema a workload owns, and the pool applies it when
	// a connection is acquired — not as a startup parameter, because datasources
	// of one physical database share one pool (see Pools) — so queries resolve
	// unqualified names against it deterministically. Empty leaves search_path
	// at the server default. It is a logical setting: two datasources sharing a
	// physical pool may (and usually do) declare different values.
	SearchPath string

	// MaxConns is the maximum number of connections in the pool.
	// Defaults to 10. A connection stating pool_max_conns wins.
	MaxConns int32

	// MinConns is the minimum number of idle connections to maintain.
	// Defaults to 2. A connection stating pool_min_conns wins; when the
	// resulting floor exceeds the ceiling, the floor is clamped to it.
	MinConns int32

	// MaxConnLifetime is the maximum lifetime of a connection.
	// Defaults to 1 hour. A connection stating pool_max_conn_lifetime wins.
	MaxConnLifetime time.Duration

	// MaxConnIdleTime is the maximum time a connection can be idle.
	// Defaults to 30 minutes. A connection stating pool_max_conn_idle_time
	// wins — that is how a test environment gets its connections released in
	// seconds instead of half an hour.
	MaxConnIdleTime time.Duration

	// HealthCheckPeriod is the interval between health checks.
	// Defaults to 1 minute. A connection stating pool_health_check_period
	// wins.
	HealthCheckPeriod time.Duration

	// ConnectTimeout is the maximum time to wait for the initial connection
	// and ping during pool creation. Defaults to 10 seconds.
	ConnectTimeout time.Duration

	// StatementTimeout is the server-side per-statement upper bound applied as
	// the Postgres `statement_timeout` runtime parameter on every connection.
	// It caps how long a single query may run before the server aborts it and
	// releases the connection, so a runaway query cannot pin a pooled
	// connection indefinitely and exhaust the pool. Defaults to 30 seconds; set
	// a negative value to disable the framework bound (no `statement_timeout`
	// runtime parameter is sent, so the connection keeps the server's
	// configured default).
	StatementTimeout time.Duration

	// IdentityResolver returns the active principal email when the DSN's user is
	// empty. Leave it nil for a socket under the Cloud SQL mount (/cloudsql/…):
	// the pool then resolves the workload's own GCP identity natively via the
	// metadata server, with a gcloud fallback for local proxies. Every other
	// host shape requires an explicit resolver when the user is absent.
	IdentityResolver func(ctx context.Context) (string, error)

	// TokenFetcher returns a per-connection password when the DSN's password is
	// empty AND the host is a Unix socket. Leave it nil for a socket under the
	// Cloud SQL mount: the pool then fetches a short-lived GCP access token per
	// connection (IAM auth) natively. Any other Unix socket requires an explicit
	// fetcher, because a bare socket may mean peer auth rather than IAM.
	TokenFetcher func(ctx context.Context) (string, error)

	// QueryObserver is an optional hook invoked once per query issued through
	// Pool.Exec/Query/QueryRow with the operation kind, the call's wall
	// duration, and the resulting error. It is the additive extension point for
	// forwarding query-duration measurements to a metrics backend; the
	// framework always emits the same measurement through the structured logger
	// regardless of whether this is set. Nil disables forwarding (logger
	// timing still applies).
	QueryObserver QueryObserver

	// TxObserver is an optional hook invoked once per transaction finalized by
	// WithTx or a request-scoped UnitOfWork on this pool, with the boundary
	// measurement (duration, retry count, committed/rolled-back outcome, and the
	// secret-free rollback cause). It is the tx-level twin of QueryObserver: the
	// additive extension point for forwarding transaction metrics
	// (sql.tx.duration / sql.tx.retries / sql.tx.outcome.* / sql.tx.rollback.*)
	// to a metrics backend. The framework always emits the same measurement on
	// the structured logger regardless of whether this is set. Nil disables
	// forwarding (logger timing still applies).
	TxObserver TxObserver

	// SlowQueryThreshold is the duration at or above which a successful query is
	// logged at Warn level as a "slow query" instead of Debug, turning query
	// latency regressions into a first-class operational signal. Zero (the
	// default) disables the slow-query escalation; normal queries are then
	// logged only at Debug. Failed queries are always logged at Warn regardless
	// of this threshold.
	SlowQueryThreshold time.Duration

	// Engine names the database engine recorded in the infra-requirements
	// manifest emitted during the build's describe phase. It defaults to
	// infra.EnginePostgres. The runtime connection is always PostgreSQL —
	// this package wraps pgx — so Engine exists only to carry the
	// deployer-facing engine into the manifest and to surface a clear
	// diagnostic when set to a value outside infra.ValidEngines.
	Engine infra.Engine

	// Database is the logical database name this pool maps to in the
	// emitted infra manifest. It defaults to the canonical default
	// datasource. Migrations registered against other datasources
	// contribute additional database entries.
	Database string

	// Schemas lists schemas this pool's code references directly, beyond
	// those derived from migration registrations. They are merged
	// additively with the migration-derived schemas for the same database.
	Schemas []string
}

func (c PoolConfig) withDefaults() PoolConfig {
	if c.MaxConns == 0 {
		c.MaxConns = 10
	}
	if c.MinConns == 0 {
		c.MinConns = 2
	}
	if c.MaxConnLifetime == 0 {
		c.MaxConnLifetime = time.Hour
	}
	if c.MaxConnIdleTime == 0 {
		c.MaxConnIdleTime = 30 * time.Minute
	}
	if c.HealthCheckPeriod == 0 {
		c.HealthCheckPeriod = time.Minute
	}
	if c.ConnectTimeout == 0 {
		c.ConnectTimeout = 10 * time.Second
	}
	if c.StatementTimeout == 0 {
		c.StatementTimeout = 30 * time.Second
	}
	return c
}

// resolveDSN returns the effective connection string, applying the
// connection precedence: an explicit DSN wins outright (the escape hatch),
// otherwise a structured Connection is rendered against the declared database
// name. An error is returned only when neither is configured, or when a
// Connection is set but underspecified.
func (c PoolConfig) resolveDSN() (string, error) {
	if dsn := strings.TrimSpace(c.DSN); dsn != "" {
		return dsn, nil
	}
	if c.Connection.isSet() {
		return c.Connection.dsn(c.databaseName())
	}
	return "", errors.Newf(CodeConnection, "PoolConfig requires a connection: set DSN or Connection")
}

// hasConnection reports whether the config carries an explicit physical
// connection — a DSN escape hatch or a structured Connection. It distinguishes
// a configured explicit-connection primary from a bare config that relies
// entirely on a binding or is unconfigured.
func (c PoolConfig) hasConnection() bool {
	return strings.TrimSpace(c.DSN) != "" || c.Connection.isSet()
}

// databaseName is the logical database a built connection targets: the
// declared datasource name when present, otherwise the legacy Database field.
func (c PoolConfig) databaseName() string {
	if name := strings.TrimSpace(c.Datasource.Name); name != "" {
		return name
	}
	return strings.TrimSpace(c.Database)
}

// Querier is the common interface for executing queries, implemented by the
// datasource-bound *PGXPool view and pgx.Tx. This allows repository methods to
// work transparently within or outside a transaction.
type Querier interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Pool is one datasource's logical pool: its search_path, its query and
// transaction telemetry, and a handle on the physical *pgxpool.Pool it acquires
// connections from. Datasources whose effective connection identity is the same
// share that physical pool (see Pools and physicalPool); every acquire through
// a *Pool carries the datasource's search_path so the connection is prepared
// for it before it is handed out (see search_path.go).
type Pool struct {
	physical *physicalPool
	log      *logger.Logger
	cfg      PoolConfig
	beginTx  func(ctx context.Context) (pgx.Tx, error) // nil → uses the physical pool's Begin
	// closed flips once Close has released this handle, so a double Close of
	// one datasource cannot release another datasource's reference and a
	// closed logical pool stops serving even while the shared physical pool
	// stays open for the others.
	closed atomic.Bool
}

// buildPoolConfig resolves the connection and renders the *pgxpool.Config the
// pool is created from: it applies the connection precedence (resolveDSN),
// explicit identity/token hooks, pool sizing, the statement_timeout runtime
// parameter and the acquire-time search_path hook — everything up to but not
// including opening the pool. NewPool calls it before connecting; tests
// exercise it directly without a live database. The receiver is assumed to
// already carry defaults (withDefaults). search_path is deliberately NOT a
// startup parameter here: the physical pool may serve several datasources, so
// it is set per acquire by PrepareConn from the acquiring datasource.
func (c PoolConfig) buildPoolConfig(ctx context.Context) (*pgxpool.Config, error) {
	dsn, err := c.resolveDSN()
	if err != nil {
		return nil, err
	}

	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, errors.Wrapf(err, CodeConnection, "parse dsn")
	}

	if !dsnSpecifiesUser(dsn) {
		resolve := c.IdentityResolver
		if resolve == nil {
			// A socket under the Cloud SQL mount can only mean IAM auth, so the
			// absent user defaults to the workload's own GCP identity. Every
			// other host shape keeps demanding an explicit resolver — an absent
			// user there is a configuration gap, not a convention.
			if !isCloudSQLSocket(poolCfg.ConnConfig.Host) {
				return nil, errors.Newf(CodeConnection, "DSN omits user: set user explicitly or provide PoolConfig.IdentityResolver")
			}
			resolve = gcpIdentityResolver
		}
		user, err := resolve(ctx)
		if err != nil {
			return nil, errors.Wrapf(err, CodeConnection, "resolve user")
		}
		poolCfg.ConnConfig.User = trimServiceAccountSuffix(strings.TrimSpace(user))
	}

	if !dsnSpecifiesPassword(dsn) {
		// pgx's ParseConfig fills Password from PGPASSWORD when the DSN omits
		// it. Override that env leak — when the user did not configure a
		// password, do not silently borrow one from the shell.
		poolCfg.ConnConfig.Password = ""

		if isUnixSocket(poolCfg.ConnConfig.Host) {
			fetch := c.TokenFetcher
			if fetch == nil {
				// Same shape rule as the user above: only the Cloud SQL mount
				// implies the IAM token-as-password convention. A bare Unix
				// socket may be peer auth, so it still requires an explicit
				// fetcher.
				if !isCloudSQLSocket(poolCfg.ConnConfig.Host) {
					return nil, errors.Newf(CodeConnection, "DSN omits password for Unix socket host: set password explicitly or provide PoolConfig.TokenFetcher")
				}
				fetch = gcpAccessTokenFetcher
			}
			poolCfg.BeforeConnect = func(ctx context.Context, conn *pgx.ConnConfig) error {
				token, err := fetch(ctx)
				if err != nil {
					return errors.Wrapf(err, CodeConnection, "fetch connection token")
				}
				conn.Password = token
				return nil
			}
		}
	}

	// Pool sizing follows the same precedence this file already applies to user
	// and password: a value the DSN states explicitly WINS, and the struct's
	// value (which by here is either the caller's or withDefaults' 10/2) fills
	// the gap. Assigning unconditionally is what made PoolConfig.MaxConns the
	// only sizing authority, because pgxpool.ParseConfig had already read
	// pool_max_conns off the DSN one moment earlier and this line discarded it.
	//
	// Nothing in production changes: no binding emits these parameters today, so
	// every existing DSN still takes the struct value it took before.
	if !dsnSpecifiesPoolOption(dsn, dsnPoolMaxConns) {
		poolCfg.MaxConns = c.MaxConns
	}
	if !dsnSpecifiesPoolOption(dsn, dsnPoolMinConns) {
		poolCfg.MinConns = c.MinConns
	}
	// MinConns above may have come from the DSN while MaxConns came from the
	// struct, or the reverse, so the pair can now disagree in a way neither
	// source could produce alone. pgxpool rejects MinConns > MaxConns at
	// construction with an error naming neither source; clamp instead, because
	// the ceiling is the constraint that was asked for and the floor is a warm-up
	// preference.
	if poolCfg.MinConns > poolCfg.MaxConns {
		poolCfg.MinConns = poolCfg.MaxConns
	}

	// The three connection-lifecycle bounds take the SAME precedence as the
	// sizing pair above, for the same reason: pgxpool.ParseConfig has already
	// read pool_max_conn_lifetime, pool_max_conn_idle_time and
	// pool_health_check_period off the DSN, and assigning unconditionally threw
	// that away — so a connection could ask for a smaller pool but never for a
	// shorter idle time, which is the half that actually RELEASES the
	// connections a test run is holding.
	if !dsnSpecifiesPoolOption(dsn, dsnPoolMaxConnLifetime) {
		poolCfg.MaxConnLifetime = c.MaxConnLifetime
	}
	if !dsnSpecifiesPoolOption(dsn, dsnPoolMaxConnIdleTime) {
		poolCfg.MaxConnIdleTime = c.MaxConnIdleTime
	}
	if !dsnSpecifiesPoolOption(dsn, dsnPoolHealthCheckPeriod) {
		poolCfg.HealthCheckPeriod = c.HealthCheckPeriod
	}

	// search_path is set per acquire, from the acquiring datasource's request,
	// so the owning schema governs unqualified name resolution from the first
	// query even when this physical pool serves several datasources.
	poolCfg.PrepareConn = prepareSearchPath

	// statement_timeout is sent as a startup runtime parameter so the server
	// aborts any single statement that runs past the bound, releasing the
	// pooled connection — a runaway query cannot pin a connection indefinitely
	// and exhaust the pool. A negative StatementTimeout disables the framework
	// bound: no statement_timeout param is sent, so the server's configured
	// default applies.
	if c.StatementTimeout >= 0 {
		ms := c.StatementTimeout.Milliseconds()
		if poolCfg.ConnConfig.RuntimeParams == nil {
			poolCfg.ConnConfig.RuntimeParams = map[string]string{}
		}
		poolCfg.ConnConfig.RuntimeParams["statement_timeout"] = strconv.FormatInt(ms, 10)
	}

	return poolCfg, nil
}

// NewPool creates a new connection pool from the given config. The pool it
// returns is a standalone logical pool over its own physical pool (nothing
// shares it; Close closes it); a workload that declares several datasources
// gets sharing through the plugin's *Pools registry instead.
//
// With no DSN user, a /cloudsql/... socket resolves the workload's GCP
// principal natively; other hosts use PoolConfig.IdentityResolver. With no DSN
// password, a /cloudsql/... socket fetches native GCP access tokens per
// connection; other Unix sockets use PoolConfig.TokenFetcher. Otherwise the
// DSN is used as-is.
func NewPool(ctx context.Context, cfg PoolConfig) (*Pool, error) {
	cfg = cfg.withDefaults()

	// Bound pool creation (connection setup + ping) by ConnectTimeout. This is
	// documented and defaulted (10s) but was previously enforced only by the
	// higher-level Pools wrapper, so a direct NewPool caller passing an
	// unbounded context got no connect bound.
	if cfg.ConnectTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.ConnectTimeout)
		defer cancel()
	}

	pool, err := connectPhysical(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return newLogicalPool(newPhysicalPool(pool, cfg, cfg.databaseName()), cfg), nil
}

// connectPhysical opens the *pgxpool.Pool for cfg (already carrying defaults)
// and verifies it with a ping acquired under cfg's search_path — the ping is an
// acquire like any other, so it must carry a datasource request or the
// PrepareConn hook fails it closed. It is the production physical opener behind
// NewPool and the *Pools registry; the registry's seam replaces it in tests.
func connectPhysical(ctx context.Context, cfg PoolConfig) (*pgxpool.Pool, error) {
	poolCfg, err := cfg.buildPoolConfig(ctx)
	if err != nil {
		return nil, err
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, errors.Wrapf(err, CodeConnection, "create pool")
	}

	if err := pool.Ping(withSearchPath(ctx, cfg.SearchPath)); err != nil {
		pool.Close()
		return nil, errors.Wrapf(err, CodeConnection, "ping")
	}
	return pool, nil
}

// newLogicalPool binds cfg's datasource to the physical pool pp, whose reference
// the caller has already taken (newPhysicalPool or physicalPool.retain).
func newLogicalPool(pp *physicalPool, cfg PoolConfig) *Pool {
	return &Pool{
		physical: pp,
		log:      databaseLoggerFrom(logger.Default()),
		cfg:      cfg,
	}
}

// acquireContext marks ctx with this datasource's search_path request. Every
// path that acquires a connection from the physical pool on behalf of this pool
// goes through it, so PrepareConn always finds the request.
func (p *Pool) acquireContext(ctx context.Context) context.Context {
	return withSearchPath(ctx, p.cfg.SearchPath)
}

// PGXPool returns the datasource-bound view of the physical pool for advanced
// usage: the pgxpool surface (Acquire, Begin, Exec, Query, CopyFrom, …) with
// every acquire carrying this datasource's search_path. It never returns the
// raw *pgxpool.Pool, which may be shared with other datasources and would hand
// out connections under whichever search_path they last served. The view is
// nil when the pool is not open, so `pool.PGXPool() != nil` keeps meaning
// "open".
func (p *Pool) PGXPool() *PGXPool {
	if p.physical == nil || p.physical.pool == nil || p.closed.Load() {
		return nil
	}
	return &PGXPool{pool: p}
}

// Acquire returns a connection from the physical pool prepared for this
// datasource's search_path. It is the explicit advanced surface for work that
// needs one session (advisory locks, LISTEN, COPY); release it with
// conn.Release. Queries, transactions and repositories do not need it.
func (p *Pool) Acquire(ctx context.Context) (*pgxpool.Conn, error) {
	return p.PGXPool().Acquire(ctx)
}

// errPoolNotOpen is the error every surface of a logical pool that is closed —
// or was never opened — reports instead of reaching the physical pool: a
// closed datasource fails its queries, it never panics and never borrows a
// connection through the shared pool its siblings still hold.
func errPoolNotOpen() error { return errors.Newf(CodeConnection, "pool is not open") }

// PGXPool is the datasource-bound view Pool.PGXPool returns: the *pgxpool.Pool
// surface advanced callers use, delegating to the physical pool the datasource
// shares with the other datasources of its database, with every acquire marked
// so the connection is prepared for this datasource's search_path first. It
// satisfies Querier. It has no Close: the logical *Pool owns its handle on the
// physical pool. A method and a type may share a name, so the view is named
// after the accessor.
//
// The nil view — what Pool.PGXPool returns once the pool is closed or before it
// opened — is safe to call: every method reports a db.connection "pool is not
// open" error through its own return shape (Stat and Config return nil), so a
// query on a closed datasource fails the way pgxpool's own closed pool did
// instead of dereferencing nil.
type PGXPool struct {
	pool *Pool
}

func (v *PGXPool) ctx(ctx context.Context) context.Context { return v.pool.acquireContext(ctx) }

func (v *PGXPool) raw() *pgxpool.Pool { return v.pool.physical.pool }

// Acquire returns a connection prepared for the datasource's search_path.
func (v *PGXPool) Acquire(ctx context.Context) (*pgxpool.Conn, error) {
	if v == nil {
		return nil, errPoolNotOpen()
	}
	return v.raw().Acquire(v.ctx(ctx))
}

// AcquireFunc acquires a connection prepared for the datasource's search_path,
// calls f with it, and releases it.
func (v *PGXPool) AcquireFunc(ctx context.Context, f func(*pgxpool.Conn) error) error {
	if v == nil {
		return errPoolNotOpen()
	}
	return v.raw().AcquireFunc(v.ctx(ctx), f)
}

// Begin starts a transaction on a connection prepared for the datasource's
// search_path.
func (v *PGXPool) Begin(ctx context.Context) (pgx.Tx, error) {
	if v == nil {
		return nil, errPoolNotOpen()
	}
	return v.raw().Begin(v.ctx(ctx))
}

// BeginTx starts a transaction with txOptions on a connection prepared for the
// datasource's search_path.
func (v *PGXPool) BeginTx(ctx context.Context, txOptions pgx.TxOptions) (pgx.Tx, error) {
	if v == nil {
		return nil, errPoolNotOpen()
	}
	return v.raw().BeginTx(v.ctx(ctx), txOptions)
}

// Exec runs sql on a connection prepared for the datasource's search_path.
func (v *PGXPool) Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	if v == nil {
		return pgconn.CommandTag{}, errPoolNotOpen()
	}
	return v.raw().Exec(v.ctx(ctx), sql, arguments...)
}

// Query runs sql on a connection prepared for the datasource's search_path.
func (v *PGXPool) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if v == nil {
		return nil, errPoolNotOpen()
	}
	return v.raw().Query(v.ctx(ctx), sql, args...)
}

// QueryRow runs sql on a connection prepared for the datasource's search_path.
// On a closed pool the error is deferred to the returned Row's Scan, pgx's own
// contract for QueryRow.
func (v *PGXPool) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if v == nil {
		return errRow{err: errPoolNotOpen()}
	}
	return v.raw().QueryRow(v.ctx(ctx), sql, args...)
}

// SendBatch sends b on a connection prepared for the datasource's search_path.
// On a closed pool the returned results report the error from every Exec,
// Query and QueryRow, and Close is nil.
func (v *PGXPool) SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults {
	if v == nil {
		return errBatchResults{err: errPoolNotOpen()}
	}
	return v.raw().SendBatch(v.ctx(ctx), b)
}

// CopyFrom bulk-loads rowSrc into tableName on a connection prepared for the
// datasource's search_path.
func (v *PGXPool) CopyFrom(ctx context.Context, tableName pgx.Identifier, columnNames []string, rowSrc pgx.CopyFromSource) (int64, error) {
	if v == nil {
		return 0, errPoolNotOpen()
	}
	return v.raw().CopyFrom(v.ctx(ctx), tableName, columnNames, rowSrc)
}

// Ping acquires a connection prepared for the datasource's search_path and
// pings it.
func (v *PGXPool) Ping(ctx context.Context) error {
	if v == nil {
		return errPoolNotOpen()
	}
	return v.raw().Ping(v.ctx(ctx))
}

// Stat returns the SHARED physical pool's statistics: every datasource of this
// database contributes to them. Nil on a closed pool.
func (v *PGXPool) Stat() *pgxpool.Stat {
	if v == nil {
		return nil
	}
	return v.raw().Stat()
}

// Config returns a copy of the shared physical pool's configuration. Its
// MaxConns is the shared pool's ceiling, and its RuntimeParams carry no
// search_path — that is set per acquire from the datasource. Nil on a closed
// pool.
func (v *PGXPool) Config() *pgxpool.Config {
	if v == nil {
		return nil
	}
	return v.raw().Config()
}

// errBatchResults is the pgx.BatchResults a closed pool's SendBatch returns:
// every result reports the fixed error and Close is a no-op, mirroring how a
// failed batch on pgx surfaces its error per result.
type errBatchResults struct{ err error }

func (r errBatchResults) Exec() (pgconn.CommandTag, error) { return pgconn.CommandTag{}, r.err }
func (r errBatchResults) Query() (pgx.Rows, error)         { return nil, r.err }
func (r errBatchResults) QueryRow() pgx.Row                { return errRow(r) }
func (r errBatchResults) Close() error                     { return nil }

// Querier returns a Querier for the current context. If a transaction begun on
// THIS pool (via WithTx) is active in the context, that transaction is
// returned. A transaction begun on a different pool is ignored — a foreign
// pool's transaction never leaks in — so the datasource-bound pool view is
// returned instead.
//
// Querier does NOT enroll the pool in a request-scoped UnitOfWork (that needs an
// error path for a failed begin); Exec/Query/QueryRow use the internal querier
// for that. It stays the pool's public, autocommit-or-explicit-tx accessor.
func (p *Pool) Querier(ctx context.Context) Querier {
	if tx := TxFromContext(ctx, p); tx != nil {
		return tx
	}
	return p.PGXPool()
}

// querier resolves the Querier a query runs through, honoring transactions in
// this precedence:
//
//  1. An explicit transaction already open on THIS pool in ctx (WithTx) wins, so
//     a repository nested inside WithTx joins that transaction.
//  2. Otherwise, a request-scoped UnitOfWork bound in ctx's DI scope: the pool is
//     enrolled (its transaction begun lazily on first use) so every repository on
//     this pool within the request shares one transaction.
//  3. Otherwise the datasource-bound pool view (autocommit).
//
// It returns an error when enrolling the pool in an active UnitOfWork fails to
// begin its transaction, and a db.connection error when the pool is closed or
// was never opened.
func (p *Pool) querier(ctx context.Context) (Querier, error) {
	if tx := TxFromContext(ctx, p); tx != nil {
		return tx, nil
	}
	if uow := unitOfWorkFromContext(ctx); uow != nil {
		tx, err := uow.enroll(ctx, p)
		if err != nil {
			return nil, err
		}
		return tx, nil
	}
	view := p.PGXPool()
	if view == nil {
		return nil, errPoolNotOpen()
	}
	return view, nil
}

// errRow is a pgx.Row whose Scan reports a fixed error. It surfaces a failed
// UnitOfWork enrollment through QueryRow's deferred-error contract instead of
// silently running the query outside the intended transaction.
type errRow struct{ err error }

func (r errRow) Scan(_ ...any) error { return r.err }

// Exec executes a query that doesn't return rows. Its wall duration and
// outcome are recorded through the pool's instrumentation (structured logger
// plus the optional QueryObserver). When a request-scoped UnitOfWork is active,
// the pool is enrolled first; a failed transaction begin is returned instead of
// running the statement outside the unit of work.
func (p *Pool) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	start := time.Now()
	q, err := p.querier(ctx)
	if err != nil {
		p.observe(ctx, QueryOpExec, start, err)
		return pgconn.CommandTag{}, err
	}
	tag, err := q.Exec(ctx, sql, args...)
	p.observe(ctx, QueryOpExec, start, err)
	return tag, err
}

// Query executes a query that returns rows. The measured duration covers
// issuing the query and obtaining the rows handle, not the caller's subsequent
// row iteration.
func (p *Pool) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	start := time.Now()
	q, err := p.querier(ctx)
	if err != nil {
		p.observe(ctx, QueryOpQuery, start, err)
		return nil, err
	}
	rows, err := q.Query(ctx, sql, args...)
	p.observe(ctx, QueryOpQuery, start, err)
	return rows, err
}

// QueryRow executes a query that returns at most one row. pgx defers the
// server round trip and any error to the returned Row's Scan, so the recorded
// measurement is emitted by the wrapped Row when Scan is called. A failed
// UnitOfWork enrollment is surfaced through the returned Row's Scan.
func (p *Pool) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	start := time.Now()
	q, err := p.querier(ctx)
	if err != nil {
		p.observe(ctx, QueryOpQueryRow, start, err)
		return errRow{err: err}
	}
	row := q.QueryRow(ctx, sql, args...)
	return observedRow{row: row, pool: p, ctx: ctx, start: start}
}

type observedRow struct {
	row   pgx.Row
	pool  *Pool
	ctx   context.Context
	start time.Time
}

func (r observedRow) Scan(dest ...any) error {
	err := r.row.Scan(dest...)
	r.pool.observe(r.ctx, QueryOpQueryRow, r.start, err)
	return err
}

// beginTransaction starts a new transaction, using the custom beginTx if set;
// otherwise it begins on a connection prepared for this datasource's
// search_path. On a closed pool it returns the db.connection error, which
// WithTx and the UnitOfWork wrap as their "begin tx" failure.
func (p *Pool) beginTransaction(ctx context.Context) (pgx.Tx, error) {
	if p.beginTx != nil {
		return p.beginTx(ctx)
	}
	return p.PGXPool().Begin(ctx)
}

// Ping verifies the connection pool is healthy, on a connection prepared for
// this datasource's search_path.
func (p *Pool) Ping(ctx context.Context) error {
	return p.PGXPool().Ping(ctx)
}

// Stats returns the SHARED physical pool's statistics: when several datasources
// of one database share the pool, every one of them contributes to the
// snapshot. Nil when the pool is not open.
func (p *Pool) Stats() *pgxpool.Stat {
	return p.PGXPool().Stat()
}

// Close releases this datasource's handle on the physical pool; the physical
// pool closes — every connection with it — when the last handle is released,
// so closing one datasource never kills another datasource's connections and a
// standalone NewPool closes its own pool. Idempotent per handle.
func (p *Pool) Close() {
	if p.physical == nil || !p.closed.CompareAndSwap(false, true) {
		return
	}
	if p.log != nil {
		p.log.Debug("closing connection pool")
	}
	p.physical.release()
}

// IsNoRows reports whether err is a pgx "no rows" sentinel.
func IsNoRows(err error) bool { return stderrors.Is(err, pgx.ErrNoRows) }

// IsConflict reports whether err is a PostgreSQL unique-violation (23505).
func IsConflict(err error) bool {
	var e *pgconn.PgError
	return stderrors.As(err, &e) && e.Code == "23505"
}

// Collect drains rows into a slice using the provided scan function.
func Collect[T any](rows pgx.Rows, scan func(pgx.Row) (T, error)) ([]T, error) {
	defer rows.Close()
	var result []T
	for rows.Next() {
		v, err := scan(rowAdapter{rows})
		if err != nil {
			return nil, err
		}
		result = append(result, v)
	}
	return result, rows.Err()
}

func isUnixSocket(host string) bool {
	return strings.HasPrefix(host, "/")
}

func trimServiceAccountSuffix(user string) string {
	return strings.TrimSuffix(user, ".gserviceaccount.com")
}

// The pgx pool parameter names — sizing and connection lifecycle. They are
// pgx's, not this package's, so they are named once here rather than spelled at
// each use.
const (
	dsnPoolMaxConns          = "pool_max_conns"
	dsnPoolMinConns          = "pool_min_conns"
	dsnPoolMaxConnLifetime   = "pool_max_conn_lifetime"
	dsnPoolMaxConnIdleTime   = "pool_max_conn_idle_time"
	dsnPoolHealthCheckPeriod = "pool_health_check_period"
)

var (
	dsnUserKeywordRe     = regexp.MustCompile(`(?i)(^|\s)user\s*=`)
	dsnPasswordKeywordRe = regexp.MustCompile(`(?i)(^|\s)password\s*=`)

	// The key=value DSN form of each pool parameter. A keyword DSN is a flat
	// string pgx has already consumed by the time buildPoolConfig looks, so the
	// only way to tell "stated" from "defaulted" is to read the raw text. Each
	// pattern anchors on a word boundary and stops at the `=`, so
	// pool_max_conn_lifetime never matches pool_max_conn_lifetime_jitter.
	dsnPoolKeywordRe = map[string]*regexp.Regexp{
		dsnPoolMaxConns:          regexp.MustCompile(`(?i)(^|\s)pool_max_conns\s*=`),
		dsnPoolMinConns:          regexp.MustCompile(`(?i)(^|\s)pool_min_conns\s*=`),
		dsnPoolMaxConnLifetime:   regexp.MustCompile(`(?i)(^|\s)pool_max_conn_lifetime\s*=`),
		dsnPoolMaxConnIdleTime:   regexp.MustCompile(`(?i)(^|\s)pool_max_conn_idle_time\s*=`),
		dsnPoolHealthCheckPeriod: regexp.MustCompile(`(?i)(^|\s)pool_health_check_period\s*=`),
	}
)

// dsnSpecifiesUser reports whether the DSN explicitly carries a user. pgx's
// ParseConfig falls back to PGUSER and the OS user when the DSN omits one,
// which makes "no user provided" undetectable post-parse — hence this raw
// inspection.
func dsnSpecifiesUser(dsn string) bool {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return false
	}
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return false
		}
		return u.User != nil && u.User.Username() != ""
	}
	return dsnUserKeywordRe.MatchString(dsn)
}

// dsnSpecifiesPassword mirrors dsnSpecifiesUser for the password field. pgx
// fills ConnConfig.Password from PGPASSWORD when the DSN omits one; the
// caller can detect "no password configured" only by inspecting the raw
// DSN before pgx hides the distinction.
func dsnSpecifiesPassword(dsn string) bool {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return false
	}
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return false
		}
		if u.User == nil {
			return false
		}
		_, hasPassword := u.User.Password()
		return hasPassword
	}
	return dsnPasswordKeywordRe.MatchString(dsn)
}

// dsnSpecifiesPoolOption reports whether the DSN carries the pgx pool parameter
// named by keyword — a sizing bound ("pool_max_conns" / "pool_min_conns") or a
// connection-lifecycle bound ("pool_max_conn_lifetime" /
// "pool_max_conn_idle_time" / "pool_health_check_period").
//
// It exists for the same reason dsnSpecifiesUser does: pgxpool.ParseConfig
// already honors these parameters, but it also fills its own defaults when they
// are absent, so post-parse the two are indistinguishable — and buildPoolConfig
// has to know whether overwriting MaxConns would discard a value the caller
// deliberately put in the DSN.
//
// This is the lever a TEST binding needs. PoolConfig.MaxConns defaults to 10 per
// datasource and MaxConnIdleTime to 30 minutes, which is right for a long-lived
// service and badly wrong for a test run: a workload with 23 datasources
// reserved 230 connections to use one or two at a time, then held every one of
// them idle for half an hour. The canonical TestBinding protocol carries no
// pool field, so a provider had no way to ask for less. It can now say so in the
// connection it hands back.
func dsnSpecifiesPoolOption(dsn, keyword string) bool {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return false
	}
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return false
		}
		return u.Query().Has(keyword)
	}
	if re, ok := dsnPoolKeywordRe[keyword]; ok {
		return re.MatchString(dsn)
	}
	return false
}
