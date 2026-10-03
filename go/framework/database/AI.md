# go.putnami.dev/database

PostgreSQL integration with pgx/v5 pool, repository pattern, query builder, migrations, and transactions.

## Quick Start

```go
import "go.putnami.dev/database"

plugin := database.NewPlugin(database.PluginConfig{
    Pool: database.PoolConfig{DSN: "postgres://user:pass@localhost:5432/mydb"},
})

a := app.New("my-service")
a.Use(plugin)
a.ListenAndServe()
```

`database.Plugin` implements `app.HealthChecker` — when mounted alongside
`go.putnami.dev/platform` (or the legacy `http.HealthPlugin`), pool
connectivity is reported under the `"database"` key on `/healthz` without
any extra wiring.

## Canonical datasource binding (recommended)

Name the logical datasource and let the deploy target inject the resolved
connection through the canonical database protocol
(`go.putnami.dev/protocol/database`):

```go
plugin := database.NewPlugin(database.PluginConfig{Datasource: "auth"})
```

At startup the plugin resolves the managed binding a deploy target injected —
a canonical `Binding` document keyed by logical datasource name — and resolves
`auth` into a pool: it builds the pgx connection from the entry's single
transport strategy (`dsn`, structured `host`/TCP, or Cloud SQL `instance`
socket) and sets the entry's `schema` as the session `search_path` of every
connection the pool hands out (set at acquire, not at connect — see *Several
datasources, one workload*). The document arrives over one of two transports: the `database` section of the
resolved application config (the control plane merges the document into that
section; operator keys are preserved, managed keys win), or the legacy
`DATABASE_BINDINGS` environment variable. Config wins when both are present —
the two are built from the same ledger during the transition, and config-first
makes the eventual env retirement a verified no-op. With no injected binding on
either transport, the plugin falls back to the named datasource's local
`PoolConfig` (`DSN` or `Connection`, plus `SearchPath`), so local dev/CI can use
the same datasource names without synthesizing a binding. When a
binding is injected, it is authoritative: a named datasource absent from that
binding fails loudly at startup with a `db.binding` diagnostic that lists the
datasources the binding does declare.

```json
{
  "$schema": "https://putnami.dev/schemas/putnami-database-binding.json",
  "protocolVersion": 1,
  "databases": {
    "auth":    { "engine": "postgres", "schema": "iam",     "connection": { "host": "localhost", "port": 5432, "database": "auth", "user": "postgres", "password": "postgres", "ssl": false } },
    "billing": { "engine": "postgres", "schema": "billing", "connection": { "instance": "proj:region:billing-db", "database": "billing" } },
    "events":  { "engine": "postgres", "schema": "events",  "connection": { "dsn": "postgres://app:s3cret@10.0.0.5:5432/events?sslmode=require" } }
  }
}
```

The binding is multi-datasource by construction. A workload that owns several
datasources declares them on one plugin via `Datasources` and resolves each pool
by name from the injected `*Pools` registry (see *Several datasources, one
workload* below) — the primary stays the unnamed `*Pool`. Bindings carry secrets
and are never written into `infra/requirements.json`; the build emits only the
thin `{name, engine}` requirement for each named datasource (schemas come from
migration sources). `PluginConfig.Pool` still supplies pool tuning (sizes,
timeouts, identity) under a named datasource, and supplies the local connection
fallback when no binding is injected.

## Several datasources, one workload (multi-pool)

A workload that owns several schemas on one physical database (or several
physical databases) gives each datasource its own logical pool — its own
`search_path` and query/transaction telemetry — by declaring them on one plugin:

```go
plugin := database.NewPlugin(database.PluginConfig{
    Datasource: "platform_core", // the default, unnamed *Pool
    Datasources: []database.DatasourceConfig{
        {Name: "platform_iam"},
        {Name: "platform_billing"},
        {Name: "analytics_events", Pool: database.PoolConfig{MaxConns: 4}}, // another database
    },
})
```

**One physical pool per database.** Datasources whose resolved connections are
identical (same host, database, user, password — the usual shape: one login,
many schemas) share ONE physical `pgxpool` (see `doc/adr/0003`). A workload
reading 21 schemas of one database holds one pool, not 21: one set of idle
backends, one `MaxConns` ceiling. What differs between those datasources is
`search_path`, and the framework sets it **per acquire**: every connection a
logical pool hands out is prepared for that datasource's schema before use
(a no-op when the connection already carries it), so unqualified queries keep
resolving in the right schema whichever datasource last used the connection.

- **Tuning is per physical database.** Every datasource that resolves to one
  database must declare the same `MaxConns`, `MinConns`, `MaxConnLifetime`,
  `MaxConnIdleTime`, `HealthCheckPeriod`, `StatementTimeout`,
  `IdentityResolver` and `TokenFetcher` (leaving all of them at their defaults
  is the common case). A difference is a `db.datasource` error naming both
  datasources and the field, raised when the second datasource's pool is first
  opened — at startup for stores the DI container builds eagerly — and the
  framework never takes the largest and never opens a second pool. In the
  example above, `MaxConns: 4` is fine only because `analytics_events` connects
  to a different database. Hooks are compared by code identity, not by closure:
  two closures of one function literal capturing different values compare
  equal and would share the pool the first one resolved, so declare hooks as
  shared functions (top-level, or one closure value reused on every datasource)
  and give a datasource that needs a different principal a different connection
  identity.
- `Pool.Stats()`, `Pool.PoolUtilization()` and `PGXPool().Stat()`/`Config()`
  describe the **shared** physical pool: `MaxConns` is the ceiling every
  datasource of that database shares.
- Do not issue a session-level `SET search_path`, `RESET search_path` or
  `DISCARD ALL` on a framework connection; transaction-local overrides
  (`SET LOCAL`, `set_config(…, true)`, what the migration runner uses) are fine.
- PostgreSQL re-plans a prepared statement when `search_path` changes, so an
  unqualified prepared query re-resolves on a reused connection. Only a
  same-named relation whose result shape differs between two schemas fails once
  with `cached plan must not change result type` — a known limit.

The plugin provides a `*Pools` registry into the container. Resolve a
datasource-bound pool by name; each is opened lazily on first use and cached
(the physical pool is opened by the first datasource to reach it and shared by
the next ones; `Pools.PhysicalCount()` reports how many databases are open):

```go
func NewBillingStore(pools *database.Pools) (*BillingStore, error) {
    pool, err := pools.For("platform_billing") // search_path = the binding's schema
    if err != nil {
        return nil, err
    }
    return &BillingStore{pool: pool}, nil
}
```

- Each `Datasources` entry resolves its connection and `search_path` from
  `DATABASE_BINDINGS` by `Name` when a binding is injected, exactly like the
  primary; without one, its `Pool` can supply a local `DSN` or `Connection`
  fallback. The build emits a `{name, engine}` infra requirement for every
  declared datasource, so each is independently provisionable.
- The primary `Datasource` remains the unnamed `*Pool` token (and is
  `Pools.Default()`), so single-datasource code, the migration runner, and the
  health probe are unchanged. Migrations run off the default pool, fanning out
  across datasources via per-transaction `search_path`.
- `pools.For` fails closed on an undeclared name, listing what the workload does
  declare. A workload may declare only `Datasources` (no primary) — then there is
  no unnamed `*Pool`; resolve everything through `Pools.For`.
- Closing one logical pool never closes another's connections: the physical
  pool closes when the last datasource sharing it is closed. `Pools.Close()`
  (the plugin's shutdown) closes everything.

**Advanced pgx access stays datasource-bound.** `pool.PGXPool()` returns a
`*database.PGXPool` view — `Acquire`, `AcquireFunc`, `Begin`, `BeginTx`,
`Exec`, `Query`, `QueryRow`, `SendBatch`, `CopyFrom`, `Ping`, `Stat`,
`Config` — that marks every acquire with this datasource's `search_path` and
delegates to the shared physical pool. It is what `pool.PGXPool().Begin(ctx)`
and `pool.PGXPool().Acquire(ctx)` have always been for advanced callers, and it
satisfies `Querier`; it is nil when the pool is not open. The raw
`*pgxpool.Pool` is never exposed: a connection acquired outside a `*Pool`
surface fails with `db.connection: connection acquired without a datasource`.
`pool.Acquire(ctx)` is the explicit one-session surface (advisory locks,
`LISTEN`, `COPY`); release it with `conn.Release()`. `Config().RuntimeParams`
carries `statement_timeout` but no `search_path`.

## Datasource and Connection (explicit escape hatch)

The **logical datasource** (what the workload owns) is decoupled from the
**physical connection** (how it connects). Without a named `Datasource`, declare
the connection in code and let the data plane supply it at bootstrap:

```go
database.PoolConfig{
    Datasource: database.Datasource{Name: "platform", Schema: "public"},
    Connection: database.Connection{Instance: "proj:region:instance"}, // Cloud SQL socket
}
```

- `Datasource{Name, Schema}` (both required when set) is what the build emits
  into `infra/requirements.json`, so a deployer resolves a real `(name, schema)`
  to provision and migrate **without parsing a DSN**. Use the same `Name` your
  migration sources target so pool and migrations fold into one entry.
- `Connection` builds the DSN against the datasource name. An `Instance`
  connection is a socket under `/cloudsql`, and that shape authenticates
  natively: empty `User` resolves the workload's GCP identity, empty `Password`
  fetches a per-connection IAM token. On any other shape, empty `User` requires
  `PoolConfig.IdentityResolver` and empty `Password` on a Unix socket requires
  `PoolConfig.TokenFetcher` (a bare socket may mean peer auth).
- A non-empty `DSN` overrides `Connection` outright — the escape hatch for
  local/dev. It never changes the emitted datasource.

Precedence: declared `{name, schema}` (code) → `Connection` (bootstrap) → `DSN`
(wins outright). Omitting `Datasource` falls back to the legacy
`Database`/`Schemas` fields.

## Statement timeout (runaway-query safety)

Every pool sends a server-side `statement_timeout` as a connection startup
runtime parameter, so Postgres aborts any single statement that runs past the
bound and releases the connection. Without it, one runaway query pins its
pooled connection indefinitely; under load this exhausts the pool (`MaxConns`
default 10) and cascades into a service-wide outage. `StatementTimeout`
**defaults to 30s** — the secure default; the bound is on by construction, not
opt-in.

```go
database.PoolConfig{
    DSN:              dsn,
    StatementTimeout: 5 * time.Second, // tighten for a latency-critical workload
}
```

- The value is applied as `statement_timeout` in **milliseconds** (a `0`
  bound, the Postgres "no limit", is never sent by default).
- Set a **negative** `StatementTimeout` to disable the bound entirely (no
  `statement_timeout` runtime param is sent — the connection keeps the server
  default). Only do this when a deliberately long-running workload (bulk
  migration, analytics) needs it, and prefer a generous explicit bound over
  disabling.
- A statement that exceeds the bound fails with Postgres `57014`
  (`query_canceled`); wrap such work in its own pool with a wider bound rather
  than disabling the protection globally.

## Pool precedence (and sizing a pool for tests)

`MaxConns` defaults to 10, `MinConns` to 2, `MaxConnLifetime` to 1 hour,
`MaxConnIdleTime` to 30 minutes and `HealthCheckPeriod` to 1 minute, per
datasource. All five can also be stated in the connection itself, as the pgx
parameters below, and **a value in the connection wins** — the same precedence
this package already applies to user and password. `PoolConfig` fills whatever
the connection leaves unsaid, bound by bound, so a connection that mentions none
of them behaves exactly as before.

| Connection parameter | Struct field | Default |
|---|---|---|
| `pool_max_conns` | `MaxConns` | 10 |
| `pool_min_conns` | `MinConns` | 2 |
| `pool_max_conn_lifetime` | `MaxConnLifetime` | 1h |
| `pool_max_conn_idle_time` | `MaxConnIdleTime` | 30m |
| `pool_health_check_period` | `HealthCheckPeriod` | 1m |

Size and idle time are two halves of one answer. A pool capped at 2 that keeps
its connections idle for the default 30 minutes still pins 2 connections per
database for a whole test run; the idle bound is what gives them back.

That is the lever a **test** environment needs. The 10-per-database default is
right for a long-lived service and wrong for a test run: datasources of one
database share a pool (see *Several datasources, one workload*), but a test
binding that hands each datasource its own isolated database reserves 10
connections per database — 23 isolated databases reserve 230 connections to use
one or two at a time, which is enough to exhaust a stock Postgres
(`max_connections` 100) and surface as `SQLSTATE 53300` while provisioning some
unrelated project's test database. A provider can ask for less through the
binding's existing `params` — no new protocol field:

```jsonc
// DATABASE_TEST_BINDINGS — a small pool per datasource, for tests only
{
  "protocolVersion": 1,
  "databases": {
    "main": {
      "engine": "postgres",
      "schema": "public",
      "connection": {
        "host": "localhost", "port": 5432, "database": "app_test",
        "user": "putnami", "password": "...",
        "params": {
          "pool_max_conns": "4", "pool_min_conns": "0",
          "pool_max_conn_idle_time": "5s"
        }
      }
    }
  }
}
```

`dbtestenv` already writes exactly these three keys into every binding it
synthesizes, so a test run gets the small pool by default and a
`PUTNAMI_TEST_PG_URL` that states a key keeps its own value.

Pick the number from what a test actually runs concurrently against one
datasource, not from the service default. Too tight shows up as a *wait* rather
than an error — a suite issuing three parallel queries on a pool of 2 gets
slower, not broken — so prefer measuring over guessing low. When a connection's
ceiling lands below the `MinConns` floor, the floor is clamped to it: the ceiling
is the constraint that was requested, the floor only a warm-up preference.

## Query metrics & pool utilization (observability)

The two primary database health signals — **query latency** and **pool
saturation** — are emitted by construction; you do not wrap call sites.

**Query duration.** Every query issued through `Pool.Exec/Query/QueryRow` (and
therefore through every `Repository` method, which routes through them) is
timed and emitted as a structured event on the existing logger:

Every record shape is built in `database_logging.go` (the twin of
`typescript/framework/database/src/observability/database-logging.ts`) so no call
site can invent a message, a severity, or a field name; the contract is normative
in `protocols/logging/conformance`. Domain fields travel in one nested,
camelCase `database` group under the pinned `database` logger:

- a successful query logs at **Debug** (`"query executed"`) with
  `database:{operation, datasource, durationMs, durationUs, outcome: "success"}`;
- a query whose duration crosses `SlowQueryThreshold` logs at **Warn**
  (`"slow query"`, adds `database.thresholdMs`; the query itself succeeded) —
  the production-visible latency signal;
- a failed query logs at **Warn** (`"query failed"`, `outcome: "failure"`) with
  the logger's **structured** `error` field. Warn, not Error: the pool reports
  outcome + latency and *propagates*, so the boundary that fails owns Error
  exactly once and a retried-and-recovered query is not error-level noise.
- `pgx.ErrNoRows` stays a Debug `"query executed"` with `outcome: "success"`.

`QueryOp`'s exported values (`exec`/`query`/`query_row`) are unchanged — they are
the `QueryObserver`/metric labels; the record renders `query_row` as `queryRow`,
the same way the exported `TxOutcome` (committed/rolled-back) renders as the
contract's `success`/`failure`.

`SlowQueryThreshold` defaults to `0` (escalation off — normal queries stay at
Debug, suppressed under the default `info` level). Set it to make latency
regressions a first-class signal:

```go
database.PoolConfig{DSN: dsn, SlowQueryThreshold: 200 * time.Millisecond}
```

To forward the same measurement to a metrics backend, set an optional
`QueryObserver` — it fires once per query with `(ctx, op, duration, err)`:

```go
database.PoolConfig{
    DSN: dsn,
    QueryObserver: func(ctx context.Context, op database.QueryOp, d time.Duration, err error) {
        histogram.Record(ctx, d.Seconds(), metric.WithAttributes(attribute.String("op", string(op))))
    },
}
```

The observer runs on the query hot path: keep it cheap, non-blocking, and
panic-free. `QueryOpQueryRow` measurements never carry an error and are a lower
bound, because pgx defers the round trip and any error to the subsequent `Scan`.

**Transaction boundaries.** Every transaction `WithTx` (or a request-scoped
`UnitOfWork`) finalizes emits one record under the same `database` logger: Debug
`"transaction committed"` with
`database:{datasource, outcome: "success", durationMs, durationUs, retries}`, or
Warn `"transaction rolled back"` with `outcome: "failure"` plus
`rollbackCause`. The cause is the **classified, secret-free** code only (a
SQLSTATE, an error class, or a sentinel such as `panic` / `unknown`) — never an
error message, a bound parameter value, or row data. The record carries no
`error` field for that reason. `TxObserver` forwards the same measurement to a
metrics backend.

**Conformance.** The database and migration records are pinned by executable
golden tests against the shared corpus in `protocols/logging/conformance`:
`logging_cross_language_test.go` here and
`typescript/framework/database/test/logging-cross-language.test.ts` in the
TypeScript runtime. The query/transaction cases drive a real pool and are gated on
`DATABASE_TEST_BINDINGS` (skipped without it); the migration cases drive the real
`Migrator` over the package's DB-free recording connection and always run. Change
a field name, a severity, or a message and the test fails until the corpus and the
other runtime are updated in the same commit.

**Pool utilization.** `Pool.Stats()` still returns the raw pgx `*pgxpool.Stat`
snapshot — of the physical pool this datasource shares with the other
datasources of its database. `Pool.PoolUtilization()` derives the gauge view
operators alert on —
acquired/idle/total/max conns, the `Utilization` ratio (acquired÷max, near `1`
means saturation), and the contention counters (`EmptyAcquireCount`,
`CanceledAcquireCount`, `EmptyAcquireWaitTime`). `Pool.LogPoolUtilization(ctx)`
emits that gauge as an Info `"pool utilization"` event, camelCase inside the same
nested `database` group as the query records. The framework spawns no
background timer (serverless-friendly); call it on your own ticker if you want
a periodic gauge.

## Repository Pattern

Repositories belong inside their **domain package** — not in a separate `persistence/`
or `repository/` folder. The domain package owns its types, repository, and service:

```go
// user/user.go — domain type + repository interface
package user

type User struct {
    ID    string `db:"id"`
    Name  string `db:"name"`
    Email string `db:"email"`
}

// Repository is the public interface — consumers depend on this.
type Repository interface {
    FindByID(ctx context.Context, id string) (User, error)
    Create(ctx context.Context, u User) error
}
```

```go
// user/pg_repo.go — Postgres implementation (unexported constructor)
package user

func scanUser(row pgx.Row) (User, error) {
    var u User
    err := row.Scan(&u.ID, &u.Name, &u.Email)
    return u, err
}

type pgRepo struct {
    *database.Repository[User]
}

func newPgRepo(pool *database.Pool) *pgRepo {
    return &pgRepo{
        Repository: database.NewRepository[User](pool, "users", scanUser),
    }
}
```

The package controls which implementation is used — callers only see the interface.
For DI registration, see `go.putnami.dev/inject`.

// Read operations
user, err := repo.FindByID(ctx, "id", userID)
users, err := repo.FindAll(ctx)                          // default limit 1000
users, err := repo.FindAllPaginated(ctx, 10, 0)          // explicit limit/offset
users, err := repo.FindWhere(ctx, "age > $1", 18)
user, err := repo.FindOneWhere(ctx, "email = $1", email)
count, err := repo.Count(ctx, "active = $1", true)
exists, err := repo.Exists(ctx, "email = $1", email)

// Delete operations
err = repo.DeleteByID(ctx, "id", userID)
n, err := repo.DeleteWhere(ctx, "active = $1", false)

// Raw queries using the repo's scan function
users, err := repo.Query(ctx, "SELECT * FROM users WHERE age > $1", 18)
user, err := repo.QueryOne(ctx, "SELECT * FROM users WHERE id = $1", id)
```

## Query Builder

Standalone query builder for INSERT, UPDATE, SELECT, DELETE:

```go
// SELECT
query, args := database.Select("users").
    Columns("id", "name", "email").
    Where("active = $1", true).
    Where("age > $1", 18).       // multiple Where() calls are ANDed
    OrderBy("name ASC").
    Limit(10).
    Offset(20).
    Build()

rows, err := pool.Query(ctx, query, args...)

// INSERT
query, args := database.Insert("users").
    Columns("name", "email").
    Values("Alice", "alice@example.com").
    Returning("id").
    Build()

// UPDATE
query, args := database.Update("users").
    Set("name = $1", "Alice").
    Set("age = $1", 30).
    Where("id = $1", userID).
    Build()

// DELETE
query, args := database.Delete("users").
    Where("active = $1", false).
    Build()
```

## Transactions

```go
err := database.WithTx(ctx, pool, func(txCtx context.Context) error {
    // All queries using txCtx share the same transaction
    _, err := pool.Exec(txCtx, "INSERT INTO users ...")
    if err != nil {
        return err // rolls back
    }
    return nil // commits
})
```

When no boundary is active, `WithTx` owns the transaction and finalizes it from
the callback result. A same-pool nested `WithTx` reuses an explicit outer
transaction. Under `PluginConfig{UnitOfWork: true}`, it instead enrolls and
joins the request unit's transaction for that pool, binds it into `txCtx`, and
leaves commit/rollback to the request boundary. This preserves visibility of
uncommitted request writes and request-wide rollback atomicity. Use
`database.InUnitOfWork(ctx)` to detect the ambient unit without beginning a
transaction.

## Migrations

Per-feature ownership. Each feature plugin embeds paired SQL files and
implements `app.MigrationContributor`:

```go
//go:embed migrations/*.sql
var migrationsFS embed.FS

func (p *Plugin) Name() string { return "iam" }

func (p *Plugin) MigrationSources() []migration.Source {
    return []migration.Source{
        database.NewSQLSource("iam", database.Datasource{Name: "default", Schema: "public"}, migrationsFS),
    }
}
```

Layout:

```
iam/migrations/
├── 20260520120000_create_users.up.sql
├── 20260520120000_create_users.down.sql   ← optional; absence = non-reversible
└── 20260601150000_add_email_index.up.sql
```

Plain SQL only — no goose markers. The framework hashes each file,
records it in `migration.migrations`, and acquires
`hashtext('putnami.migration:<datasource>')` per datasource (shared
with the TS runner).

Status and drift inspection are strictly read-only. They do not create the
state store or acquire the writer lock, so a restricted application runtime
role can inspect migration readiness without DDL privileges. A state store that
does not exist yet is treated as empty; registered definitions therefore remain
pending until the dedicated migration path applies them.

Every migration also emits one **terminal record** under the pinned
`database.migration` logger: Info `"migration applied"` with
`migration:{name, datasource, durationMs, outcome: "success"}`, or Error
`"migration failed"` with `outcome: "failure"` plus the structured cause (the
apply still returns the error, so the run aborts). Bookkeeping records — the
advisory-lock release warning, the failure-row warning — stay separate signals.

### Decision tree

```
Adding a schema change?
└── Drop NNN_name.up.sql into your feature's migrations/.
    Reversible? Drop a sibling .down.sql.

Running migrations in a job?
└── Use the service's cmd/migrate binary (~3 lines, see
    go.putnami.dev/migratecli). It reuses wire.BuildApp, no HTTP
    listener starts, every feature's migrations are picked up.

Adding a new feature plugin?
└── Implement app.MigrationContributor.MigrationSources(). Done.

Wondering whether a migration was picked up?
└── Run `./migrate inspect` (no DB access), or enable debug logging:
    every contribution emits a structured `database.migration` event at debug.
```

### Migration bundles

`putnami build` emits a self-contained, content-addressed migration bundle to
`.gen/migration-bundle/` (a `bundle.json` manifest plus a `payload/` tree of
`.up.sql`/`.down.sql` files). The SQL runner contributes it via
`protocolmigration.BundleContributor`; the describe phase assembles and writes
it. Bundles are immutable release artifacts — no secrets, DSNs, or credentials.

```
Publishing a bundle?
└── `putnami cloud publish-migration` (add `--dry-run` to preview)
    validates, hash-verifies payloads, and uploads the complete bundle.

Executing a published bundle (no app graph / service image)?
└── database.ApplyBundle(ctx, pool, os.DirFS(".gen/migration-bundle")).
    LoadBundleSources reconstructs the SQLSources first; the reconstruction is
    exact (same names, SQL, datasources, schemas, hashes), so it applies
    identically to running from source — including setting each datasource's
    declared schema as the search_path, so one bundle routes itself into many
    schemas with no per-pool wiring.
```

The bundle protocol and its digest live in `go.putnami.dev/protocol/migration`;
the digest is byte-for-byte identical to the TypeScript runtime's.

### One migration set → many schemas

`Datasource{Name, Schema}` is self-describing at apply time: when a source
declares a `Schema`, the per-datasource Migrator sets it as the
transaction-local `search_path` (`set_config('search_path', …, true)`) before
each up/down body, then reverts at commit. Write **unqualified DDL**
(`CREATE TABLE users (…)`, never `CREATE TABLE iam.users (…)`) and the target
schema is chosen by configuration, not baked into the SQL. The
`migration.migrations` state store is fully qualified, so the search_path never
disturbs bookkeeping.

To point one migration set at several schemas, register it under several
datasources, each owning its schema:

```go
func (p *Plugin) MigrationSources() []migration.Source {
    var out []migration.Source
    for _, proto := range []string{"put", "oci", "npm", "gomod"} {
        out = append(out, database.NewSQLSource(
            "registry",
            database.Datasource{Name: "registry_" + proto, Schema: "registry_" + proto},
            registryMigrationsFS, // one shared, schema-agnostic migration set
        ))
    }
    return out
}
```

The runner fans out one Migrator per datasource, each applying into its own
schema through a **single pool** — no per-schema connection needed. The
datasource **names must differ**: `migration.migrations` keys each row on
`(datasource, name)`, so reusing one name across schemas would make the second
target look already-applied and silently skip it. Two sources mapping the same
datasource to different schemas is rejected at materialization, and earlier by
the build: the bundle describer runs the same `migration.ResolveDatasourceSchemas`
check and fails `putnami build`, naming both sources' namespaces. The owning
schemas are also what `infra/requirements.json` declares, so a deployer
provisions them before migrations run.

This travels through the **published bundle** too: the declared schema is part
of each bundle operation, so `database.ApplyBundle(ctx, pool, bundleFS)`
reconstructs schema-aware sources and routes every datasource into its own
schema automatically — no `PoolConfig.SearchPath` and no per-schema apply loop.
A schema-less operation omits the field (digest-identical to before), leaving
the connection's own search_path in force.

**Override — per-pool search_path.** To apply a bundle to a *different* schema
than it declares (e.g. one schema-agnostic bundle fanned out per tenant), bind
the pool's search_path and the runner-set value steps aside only for sources
that declare none; for declared-schema sources, re-target them or publish them
schema-less:

```go
pool, _ := database.NewPool(ctx, database.PoolConfig{DSN: dsn, SearchPath: "tenant_42"})
database.ApplyBundle(ctx, pool, os.DirFS(".gen/migration-bundle")) // schema-less bundle → lands in tenant_42
```

`PoolConfig.SearchPath` (or the `schema` of a `DATABASE_BINDINGS` entry) makes
the schema the search_path of every connection the pool hands out — the
runner's `database/sql` handle acquires each of its connections through the
pool's datasource, even on a physical pool shared with other datasources — and a
schema-less bundle's unqualified DDL lands in it.

See `doc/getting-started.md` and the worked example at
`go/samples/migrations-feature/` for the full reference.

## Test database provider

`go.putnami.dev/database/testprovider` turns a canonical **test binding**
(`go.putnami.dev/protocol/database` `TestBinding`) into isolated, migrated
databases for one or more named datasources — the Go half of the uniform Go/TS
test-DB contract. It targets an externally provided Postgres (CI service
container or a local server): the environment injects the binding via
`DATABASE_TEST_BINDINGS`, and the provider creates an isolated database (or
schema) per datasource, applies the published migration bundle, and returns a
runtime `Binding` the adapter consumes through `database.PoolConfigFromBinding`.

```go
res, err := testprovider.Provision(ctx, testprovider.Options{
    Bundle: os.DirFS(".gen/migration-bundle"), // applied when the binding sets applyMigrations
})
if errors.Is(err, testprovider.ErrSkip) {
    t.Skip("no test database binding")
}
defer res.Cleanup() // drops the isolated databases/schemas

cfg, _ := database.PoolConfigFromBinding(res.Binding, "auth")
pool, _ := database.NewPool(ctx, cfg) // connected to the isolated, migrated "auth" DB
```

Policy comes from the binding: `mode` (`require` fails loudly when no binding is
present, `skip` returns `ErrSkip`, and `auto` currently has no Docker implementation),
`isolation` (`database` | `schema`), `applyMigrations`, `reuse`, and
`keepDatabases` (skip the per-suite `DROP DATABASE` when the server's own
lifetime is the cleanup — a DROP forces a cluster-wide checkpoint, so on a busy
shared server it stalls behind the whole fleet's writes). Each
datasource's database receives only its own migrations (the bundle is scoped by
target datasource), and it is multi-datasource by construction — no single-DSN
env convention. Unit tests pay no database cost (the live path runs only when
`DATABASE_TEST_BINDINGS` is set).

`Options.Datasources` names the datasources a suite uses. When set, `Provision`
plans, builds templates for, reclaims, provisions and tears down only those
entries, and `Result.Binding` carries only them, so a suite handed a
workspace-wide binding pays for the datasources it reads. A name the binding
lacks follows the binding's mode, as a missing binding does: `skip` returns an
error that wraps `ErrSkip`, `require` and `auto` fail and name the datasource.
Empty provisions every datasource. The TypeScript provider's `datasources`
option behaves the same; both run the shared corpus in
`protocols/database/conformance/test-provider-datasources.json`.

```go
res, err := testprovider.Provision(ctx, testprovider.Options{Datasources: []string{"auth"}})
```

With `reuse: bundle-template` (and database isolation), migrations are applied
once into a **template database keyed by the migration-bundle digest**; each
suite is then a fast `CREATE DATABASE … TEMPLATE …` clone rather than a
migration replay. Template creation is serialized across concurrent
suites/processes with a Postgres advisory lock, and a changed bundle (new
digest) gets a fresh template. Per-suite databases are dropped deterministically
on `Cleanup`; the template is left warm and is immutable (only cloned from), so
it leaks no cross-suite state. `Cleanup` does not use the context `Provision`
received, so provisioning with `t.Context()` (canceled before cleanups run)
still drops the databases.

A killed test process never runs `Cleanup`. A per-suite database is named
`<base>_t_<creation time><random>` (8 + 8 lowercase hex digits; the time is Unix
seconds), and with database isolation and without `keepDatabases`, `Provision`
first drops the per-suite databases of the same base that have no open
connection and are older than one hour — at most 16 per call, without `FORCE`,
under a try-advisory-lock so concurrent suites never reclaim the same base at
once. A live suite's database is never dropped. Names without a creation time
(written before this format) are never reclaimed. The TypeScript provider uses
the same format and rule. Docker `mode: auto` is currently unsupported.

## Contract invariants

- Transactions are bound to one pool; every finalization path commits once or
  rolls back without leaving a reusable half-finalized handle.
- A unit of work makes one datasource atomic and does not promise distributed
  atomicity across datasource groups.
- Telemetry identifies the datasource and operation without emitting DSNs,
  credentials, or bound values.

Support is **stable** in `../../../putnami.support.json`; the normative feature
contract is `specs/persistence-transactions.json`, with the decision in
`doc/adr/0001-pool-scoped-transaction-boundaries.md`.
