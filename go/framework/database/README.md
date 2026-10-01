# Database & PostgreSQL

The `database` package provides PostgreSQL integration built on [pgx/v5](https://pkg.go.dev/github.com/jackc/pgx/v5), with connection pooling, transaction management, a generic repository pattern, schema migrations, and a query builder. Migrations follow the cross-language Putnami migration protocol — TypeScript and Go runners share one state store and one advisory lock per datasource.

## Connection Pool

Create a connection pool with `NewPool`:

```go
import "go.putnami.dev/database"

pool, err := database.NewPool(ctx, database.PoolConfig{
    DSN:             "postgres://user:pass@localhost:5432/mydb?sslmode=disable",
    MaxConns:        10,
    MinConns:        2,
    MaxConnLifetime: time.Hour,
    MaxConnIdleTime: 30 * time.Minute,
})
defer pool.Close()
```

The pool verifies connectivity on creation. Use `pool.Ping(ctx)` for health checks and `pool.Stats()` for the raw pgx statistics. For observability, every query is timed and emitted on the structured logger (Warn on a failed query or one over `PoolConfig.SlowQueryThreshold`, Debug otherwise), with an optional `PoolConfig.QueryObserver` to forward `(op, duration, err)` to a metrics backend; `pool.PoolUtilization()` derives the saturation gauge (utilization ratio plus contention counters) and `pool.LogPoolUtilization(ctx)` emits it. See `AI.md` for details.

### Cloud SQL on GCP

Cloud SQL IAM identity is native for the one host shape that implies it: a
socket under the `/cloudsql` mount with no user and no password resolves the
workload's GCP identity (metadata server, `gcloud` fallback) and fetches a
short-lived IAM access token per connection — no import, no hook.

```go
pool, err := database.NewPool(ctx, database.PoolConfig{
    DSN: "host=/cloudsql/my-project:europe-west1:my-instance dbname=auth sslmode=disable",
})
```

On Cloud Run, deploy with `--add-cloudsql-instances=<instance>` so GCP mounts a
Unix socket at `/cloudsql/<instance>/.s.PGSQL.5432`. Any other shape keeps the
explicit hooks: when the DSN omits the user, set `IdentityResolver`; when it
omits the password on a non-Cloud-SQL Unix socket, set `TokenFetcher` — an
absent password there may mean peer auth, so nothing is assumed.

On a dev laptop, run `cloud-sql-proxy` against the same Cloud SQL instance on a
TCP port, point the DSN at `localhost:6543`, and let the proxy handle auth. A
TCP DSN with an explicit user does not require `TokenFetcher`.

## Transactions

Wrap operations in a transaction with `WithTx`. When no transaction boundary is
already active, `WithTx` opens one, commits on success, and rolls back on error.
Nested calls reuse the existing transaction:

```go
err := database.WithTx(ctx, pool, func(ctx context.Context) error {
    // All queries in this block use the same transaction
    _, err := pool.Exec(ctx, "INSERT INTO users (name) VALUES ($1)", "Alice")
    if err != nil {
        return err // triggers rollback
    }
    _, err = pool.Exec(ctx, "INSERT INTO audit_log (action) VALUES ($1)", "user_created")
    return err // nil triggers commit
})
```

The transaction is stored in `context.Context`. Any code that accepts the context—including repository methods—automatically participates in the transaction.

### Request-scoped Unit of Work

Enable a DI request-scoped transaction with `PluginConfig.UnitOfWork`:

```go
app.New("my-service").
    Use(database.NewPlugin(database.PluginConfig{
        Datasource: "app",
        UnitOfWork: true,                       // one transaction per datasource per request
        UnitOfWorkTimeout: 5 * time.Second,     // bounds each commit/rollback (0 = unbounded)
    }))
```

With it on, every repository query on a pool within an HTTP request scope transparently joins one transaction per participating datasource — no explicit `WithTx` and no context threading. The request-scope boundary **commits on handler success and rolls back on handler error, a 5xx response, a canceled/timed-out request, or a panic**, always releasing every opened connection. A handler that must roll back while still returning a non-error response calls `SetRollbackOnly()` on the injected `*database.UnitOfWork`. A 4xx response commits (it is a handled outcome).

**Multi-datasource is best-effort, NOT two-phase commit.** A unit of work spanning one datasource is atomic. Spanning several datasources, `Commit` commits each in enrollment order; a commit failure on datasource *N* — after datasources before it already committed — leaves those **committed** (a documented partial commit), rolls back the rest to release their connections, and surfaces an error naming how many committed. The framework never pretends cross-datasource atomicity; workloads needing it must not span datasources in one unit of work.

Nesting is symmetric. A unit of work enrolling a pool that already carries an
explicit `WithTx` transaction joins it (no savepoints) and does not own its
commit/rollback. Conversely, `WithTx` called inside an active request unit
enrolls and joins the unit's transaction for that pool: it can see uncommitted
request writes and does not commit or roll back early. It only returns the
callback result; the request boundary remains the finalization owner. Use
`InUnitOfWork(ctx)` to detect whether a context has a request unit without
beginning a transaction.

## Repository Pattern

`Repository[T]` provides generic CRUD operations. Supply a scan function to map rows to your entity type:

```go
type User struct {
    ID    int
    Name  string
    Email string
}

func scanUser(row pgx.Row) (User, error) {
    var u User
    err := row.Scan(&u.ID, &u.Name, &u.Email)
    return u, err
}

repo := database.NewRepository[User](pool, "users", scanUser)

// Find by ID
user, err := repo.FindByID(ctx, "id", 42)

// Find with conditions
active, err := repo.FindWhere(ctx, "active = $1 AND age > $2", true, 18)

// Count
count, err := repo.Count(ctx, "active = $1", true)

// Check existence
exists, err := repo.Exists(ctx, "email = $1", "alice@example.com")

// Delete
err = repo.DeleteByID(ctx, "id", 42)

// Raw queries
users, err := repo.Query(ctx, "SELECT * FROM users ORDER BY created_at DESC LIMIT $1", 10)
```

Repositories automatically use the current transaction when called within `WithTx`.

## Query Builder

Build parameterized queries with a fluent API:

```go
// SELECT
query, args := database.Select("users").
    Columns("id", "name", "email").
    Where("age > $1", 18).
    Where("active = $1", true).
    OrderBy("name ASC").
    Limit(10).
    Offset(20).
    Build()
// SELECT id, name, email FROM users WHERE age > $1 AND active = $2 ORDER BY name ASC LIMIT 10 OFFSET 20

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
    Where("id = $1", 42).
    Build()

// DELETE
query, args := database.Delete("users").
    Where("active = $1", false).
    Returning("id").
    Build()
```

Positional parameters (`$1`, `$2`, ...) are automatically rewritten across chained calls, so each clause can use `$1` locally.

## Migrations

Migrations live inside the feature package that owns them, as paired SQL files
(`<name>.up.sql` and an optional `<name>.down.sql`). A feature plugin embeds
its `migrations/` directory and contributes it as a `SQLSource` by implementing
`app.MigrationContributor`. The framework collects every source into the
per-app migration registry; the SQL runner pairs the files, hashes them, and
applies the pending entries — one `Migrator` per datasource, in lexicographic
order.

```go
package iam

import (
    "embed"

    "go.putnami.dev/database"
    "go.putnami.dev/migration"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

type Plugin struct{}

func New() *Plugin { return &Plugin{} }

func (p *Plugin) Name() string { return "iam" }

// MigrationSources is invoked during the Migrate lifecycle phase. The
// framework feeds the returned sources into the per-app migration registry.
func (p *Plugin) MigrationSources() []migration.Source {
    return []migration.Source{
        database.NewSQLSource("iam", database.Datasource{Name: "default", Schema: "public"}, migrationsFS),
    }
}
```

Inline definitions can be appended after the embedded FS for rare one-off boot
data:

```go
database.NewSQLSource("iam", database.Datasource{Name: "default", Schema: "public"}, migrationsFS,
    database.Definition{Name: "20260601_seed_root_role", SQL: "INSERT INTO ..."},
)
```

Migration state lives in the canonical `migration.migrations` table (shared
with the TypeScript runner). Each apply and rollback is wrapped in a single
Postgres transaction, and a session-level advisory lock
(`pg_advisory_lock(hashtext('putnami.migration:<datasource>'))`) prevents
concurrent runners from racing on the same datasource. The `down` SQL is
persisted alongside its hash, so a rollback still works after the code that
defined the migration has been removed.

### Direct Migrator use (tests)

For tests that want a single-purpose engine without the plugin lifecycle,
construct a `Migrator` with an explicit definition slice:

```go
migrator := database.NewMigrator(stdDB, database.MigrationConfig{
    Datasource: "default", // optional; resolves to "default" when empty
    Definitions: []database.Definition{{
        Name: "iam/20260512120000_create_users",
        SQL:  `CREATE TABLE users (id TEXT PRIMARY KEY, name TEXT NOT NULL);`,
        Down: `DROP TABLE users;`,
    }},
})

applied, err := migrator.Up(ctx)    // Run pending migrations
last, err := migrator.Rollback(ctx) // Roll back the most recent migration
status, err := migrator.Status(ctx) // Read recorded migrations without DDL or a writer lock
err = migrator.Reset(ctx)           // Roll back every applied migration
```

`Status` and drift inspection are safe for the restricted application runtime
role: they never create `migration.migrations` and never acquire the migration
writer lock. Before the dedicated migration job has created the state store,
status treats it as empty, so registered definitions appear as pending. Apply
and rollback remain the only paths that initialize the store.

See [`doc/getting-started.md`](doc/getting-started.md) for the full authoring
and CLI workflow.

## Plugin Integration

Use `Plugin` to integrate with the application lifecycle:

```go
app.New("my-service").
    Use(database.NewPlugin(database.PluginConfig{
        Pool: database.PoolConfig{
            DSN: os.Getenv("DATABASE_URL"),
        },
        // AutoApply runs the contributed migrations during the framework's
        // Migrate phase. Leave it false in production and drive migrations
        // from a dedicated migrate CLI/job instead.
        Migration: &database.MigrationConfig{AutoApply: true},
    })).
    Use(iam.New()) // feature plugins contribute their SQLSources
```

The plugin registers `*database.Pool` (the primary/default datasource) in the DI
container and, during `Configure`, registers an `SQLRunner` into the per-app
`*migration.Registry`. That runner consumes every `SQLSource` contributed by
feature plugins (via `app.MigrationContributor`) and fans out across the
datasources it discovers. The framework's Migrate phase then drives the runner
when `AutoApply` is set; the pools are closed on shutdown.

A workload that owns several datasources declares them with `PluginConfig.Datasources`
and resolves each datasource-bound pool by name from the injected `*database.Pools`
registry (`pools.For("platform_iam")`) — each logical pool carries its own
`search_path`, opened lazily on first use. Datasources that resolve to the same
physical connection (several schemas of one database) share one physical
connection pool and must declare the same tuning; the framework sets each
datasource's `search_path` when it hands a connection out, so unqualified
queries keep resolving in their own schema. `pool.PGXPool()` returns a
datasource-bound view of that shared pool, never the raw `pgxpool`. Injected
managed bindings are authoritative — the binding document carried by the
`database` section of the resolved config, falling back to the legacy
`DATABASE_BINDINGS` env var (config wins when both are present); without one, a
named datasource can fall back to its local `PoolConfig` DSN/Connection and
`SearchPath`. The primary `Datasource` stays the unnamed `*Pool`, so
single-datasource code is unchanged. See AI.md → *Several datasources, one
workload*.

The plugin also implements `app.HealthChecker`: when `http.HealthPlugin` is
mounted in the same application, it auto-discovers this probe and reports
pool connectivity (via `pool.Ping`) under the `"database"` key on `/_/health`.
No manual `AddChecker` wiring is needed.

## Infrastructure Requirements

The plugin implements `app.Describer`: during the build's describe phase it
emits a framework-generated infra-requirements scratch fragment at
`<project>/.gen/infra/database.json` declaring the `databases` the project
needs. The Go generator syncs that fragment into the committed
`<project>/infra/requirements.json`; `putnami build` then merges committed
requirements into the workload's ephemeral `.gen/requirements.json`.

One `databases` entry is emitted per logical datasource:

- The pool's own database — named by `PoolConfig.Database` (default `"default"`),
  with `PoolConfig.Engine` (default `postgres`, validated against the infra
  protocol's engine set) and `PoolConfig.Schemas`.
- Every datasource targeted by a registered SQL migration, whose namespaces
  become that database's schemas.

Schemas for the same database are merged into a sorted union, so two libraries
that migrate the same datasource contribute their schemas additively.

```go
Pool: database.PoolConfig{
    DSN:      os.Getenv("DATABASE_URL"),
    Database: "primary",      // → databases[].name
    Schemas:  []string{"app"}, // merged with migration namespaces
},
```

## Support and contract

The SDD owner is `go`. `go.putnami.dev/database` is public, documented,
maintained, and classified **stable** in the repository
[support catalog](../../../putnami.support.json). Before v1.0, minor releases
may still require documented migrations; see the repository
[release policy](../../../RELEASE.md).

The durable transaction and unit-of-work contract is
[`go/persistence-transactions`](specs/persistence-transactions.json). Pool-scoped
boundaries and the absence of cross-datasource atomicity are recorded in
[ADR 0001](doc/adr/0001-pool-scoped-transaction-boundaries.md) and protected by
[`database_test.go`](database_test.go), [`repository_test.go`](repository_test.go),
and [`unit_of_work_test.go`](unit_of_work_test.go).
