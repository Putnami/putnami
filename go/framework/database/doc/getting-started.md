# Database -- PostgreSQL Repository Layer

`go.putnami.dev/database` provides PostgreSQL integration for the Putnami Go framework. It wraps [pgx/v5](https://pkg.go.dev/github.com/jackc/pgx/v5) for connection pooling, adds context-based transaction management, a generic repository pattern with type-safe CRUD operations, a fluent query builder, and schema migrations aligned with the cross-language Putnami migration protocol (shared state store and advisory locking with the TypeScript runner).

## Installation

Add the module to your Go project:

```go
import "go.putnami.dev/database"
```

## Database Connection

### Plugin Setup

The simplest way to integrate PostgreSQL is through the `database.Plugin`, which manages the connection pool lifecycle and registers it in the dependency injection container.

```go
package main

import (
    "go.putnami.dev/app"
    "go.putnami.dev/database"
)

func main() {
    app.New("my-service").
        Use(database.NewPlugin(database.PluginConfig{
            Pool: database.PoolConfig{
                DSN: "postgres://user:pass@localhost:5432/mydb?sslmode=disable",
            },
        })).
        Run()
}
```

The plugin:

1. **Provides** -- registers `*database.Pool` as a DI provider.
2. **Configure** -- resolves the pool and runs migrations before the app is ready when configured.
3. **Stop** -- closes all connections in the pool.

### Pool Configuration

`PoolConfig` controls connection pool behavior. All fields except `DSN` have sensible defaults:

```go
database.PoolConfig{
    DSN:               "postgres://user:pass@localhost:5432/mydb?sslmode=disable",
    MaxConns:          10,              // Maximum connections (default: 10)
    MinConns:          2,               // Minimum idle connections (default: 2)
    MaxConnLifetime:   time.Hour,       // Max lifetime per connection (default: 1h)
    MaxConnIdleTime:   30 * time.Minute, // Max idle time per connection (default: 30m)
    HealthCheckPeriod: time.Minute,     // Health check interval (default: 1m)
}
```

### Declaring the Datasource

A workload's **logical datasource** — the database it owns — is separate from
the **physical connection** it uses to reach it. Declare the datasource in
code with a name and the schema it owns. A Cloud SQL socket connection needs
nothing else — IAM identity is the native default for that shape:

```go
database.PoolConfig{
    Datasource: database.Datasource{Name: "platform", Schema: "public"},
    Connection: database.Connection{Instance: "my-proj:europe-west1:pg"},
}
```

The declared `(name, schema)` is what the build emits into
`infra/requirements.json`, so a deployer can provision and migrate against a
resolvable datasource **without parsing a connection string** — even when the
runtime connects via a raw DSN. Use the same `name` your migration sources
target (see [Wiring a Feature Plugin](#wiring-a-feature-plugin)) so the pool
and its migrations fold into one requirement entry instead of a split between
a generic `default` pool database and a named migration database.

Both `Name` and `Schema` are required when `Datasource` is set; a partial
declaration fails the build's describe phase. Omitting `Datasource` entirely
falls back to the legacy `Database`/`Schemas` fields.

### Connection Override at Bootstrap

The connection is resolved with a fixed precedence — declared datasource
(code) → `Connection` (data plane / env at bootstrap) → `DSN` (escape hatch,
wins outright):

```go
// Code declares the datasource; the data plane supplies the connection.
database.PoolConfig{
    Datasource: database.Datasource{Name: "platform", Schema: "public"},
    Connection: database.Connection{
        Instance: "my-proj:europe-west1:pg", // Cloud SQL socket /cloudsql/<instance>
        // Host/Port/User/Password/Params for TCP or local connections.
    },
}
```

`Connection` builds a DSN against the declared datasource name. An `Instance`
connection renders as a socket under `/cloudsql`, and that shape authenticates
natively: an empty `User` resolves the workload's GCP identity (metadata
server, `gcloud` fallback) and an empty `Password` fetches a per-connection
IAM access token. On every other shape, leaving `User` empty requires
`PoolConfig.IdentityResolver`, and leaving `Password` empty on a Unix socket
requires `PoolConfig.TokenFetcher` — a bare socket may mean peer auth, so
nothing is assumed there. A non-empty `DSN` overrides `Connection` outright
and stays the right choice for local/dev or special datasources:

```go
database.PoolConfig{
    Datasource: database.Datasource{Name: "platform", Schema: "public"},
    DSN:        "postgres://user:pass@localhost:5432/platform?sslmode=disable",
}
```

The `DSN` never changes the datasource the build emits — that always comes
from `Datasource` (or the legacy `Database`).

### Direct Pool Creation

For cases where you need a pool outside the plugin system:

```go
pool, err := database.NewPool(ctx, database.PoolConfig{
    DSN: "postgres://user:pass@localhost:5432/mydb?sslmode=disable",
})
if err != nil {
    log.Fatal(err)
}
defer pool.Close()
```

`NewPool` parses the DSN, applies defaults, creates the connection pool, and verifies connectivity with a ping.

### Executing Raw Queries

The `Pool` type implements the `Querier` interface and is transaction-aware. When a transaction is active in the context, all queries automatically route through it:

```go
// Execute a query that doesn't return rows
_, err := pool.Exec(ctx, "UPDATE users SET active = $1 WHERE id = $2", true, 42)

// Execute a query that returns multiple rows
rows, err := pool.Query(ctx, "SELECT id, name FROM users WHERE active = $1", true)

// Execute a query that returns a single row
row := pool.QueryRow(ctx, "SELECT name FROM users WHERE id = $1", 42)
var name string
err := row.Scan(&name)
```

### The Querier Interface

Both `*Pool` and `pgx.Tx` implement the `Querier` interface, which means repository and query code works identically inside or outside a transaction:

```go
type Querier interface {
    Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
    Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
    QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}
```

Use `pool.Querier(ctx)` to get the appropriate querier for the current context -- it returns the active `WithTx` transaction on this pool if one exists, or the pool otherwise. It does not join a request-scoped unit of work; `pool.Exec`, `pool.Query` and `pool.QueryRow` do.

## Repositories

The generic `Repository[T]` provides type-safe CRUD operations for a database table. You supply a table name and a scan function that maps a row to your entity type.

### Defining a Repository

```go
package user

import (
    "github.com/jackc/pgx/v5"
    "go.putnami.dev/database"
)

type User struct {
    ID    int64
    Name  string
    Email string
    Age   int
}

func scanUser(row pgx.Row) (User, error) {
    var u User
    err := row.Scan(&u.ID, &u.Name, &u.Email, &u.Age)
    return u, err
}

type UserRepo struct {
    *database.Repository[User]
}

func NewUserRepo(pool *database.Pool) *UserRepo {
    return &UserRepo{
        Repository: database.NewRepository[User](pool, "users", scanUser),
    }
}
```

### CRUD Operations

**Find by ID:**

```go
user, err := repo.FindByID(ctx, "id", 42)
```

**Find all rows:**

```go
users, err := repo.FindAll(ctx)
```

**Find with conditions:**

```go
users, err := repo.FindWhere(ctx, "age > $1 AND active = $2", 18, true)
```

**Find a single matching row:**

```go
user, err := repo.FindOneWhere(ctx, "email = $1", "alice@example.com")
```

**Count rows:**

```go
// Count all
total, err := repo.Count(ctx, "")

// Count with condition
active, err := repo.Count(ctx, "active = $1", true)
```

**Check existence:**

```go
exists, err := repo.Exists(ctx, "email = $1", "alice@example.com")
```

**Delete by ID:**

```go
err := repo.DeleteByID(ctx, "id", 42)
```

**Delete with conditions:**

```go
deleted, err := repo.DeleteWhere(ctx, "active = $1", false)
// deleted = number of rows removed
```

**Execute raw queries through the repository:**

```go
users, err := repo.Query(ctx, "SELECT * FROM users WHERE name ILIKE $1 ORDER BY name", "%alice%")

user, err := repo.QueryOne(ctx, "SELECT * FROM users WHERE id = $1 FOR UPDATE", 42)
```

### Adding Custom Methods

Embed the generic repository and add domain-specific methods:

```go
type UserRepo struct {
    *database.Repository[User]
}

func (r *UserRepo) FindByEmail(ctx context.Context, email string) (User, error) {
    return r.FindOneWhere(ctx, "email = $1", email)
}

func (r *UserRepo) FindActive(ctx context.Context) ([]User, error) {
    return r.FindWhere(ctx, "active = $1", true)
}

func (r *UserRepo) Create(ctx context.Context, name, email string) (User, error) {
    q := database.Insert(r.Table()).
        Columns("name", "email").
        Values(name, email).
        Returning("id", "name", "email", "age")

    query, args := q.Build()
    return r.QueryOne(ctx, query, args...)
}
```

## Query Builder

The `QueryBuilder` constructs parameterized SQL queries with a fluent API. `Build` quotes the table and the columns you name, never a condition. It handles positional parameter rewriting automatically -- each `.Where()` or `.Set()` call uses `$1`-based numbering locally, and the builder renumbers them in the final query.

### SELECT

```go
q, args := database.Select("users").
    Columns("id", "name", "email").
    Where("age > $1", 18).
    Where("active = $1", true).
    OrderBy("name ASC").
    Limit(10).
    Offset(20).
    Build()
// q    = `SELECT "id", "name", "email" FROM "users" WHERE age > $1 AND active = $2 ORDER BY "name" ASC LIMIT 10 OFFSET 20`
// args = [18, true]
```

Omit `.Columns()` to select all columns (`SELECT *`).

**Aggregation with GROUP BY:**

```go
q, args := database.Select("orders").
    Columns("status", "COUNT(*) as count").
    Where("created_at > $1", "2024-01-01").
    GroupBy("status").
    Having("COUNT(*) > 5").
    Build()
// q    = `SELECT "status", COUNT(*) as count FROM "orders" WHERE created_at > $1 GROUP BY "status" HAVING COUNT(*) > 5`
// args = ["2024-01-01"]
```

### INSERT

```go
// Single row
q, args := database.Insert("users").
    Columns("name", "email").
    Values("Alice", "alice@example.com").
    Returning("id", "created_at").
    Build()
// q    = `INSERT INTO "users" ("name", "email") VALUES ($1, $2) RETURNING "id", "created_at"`
// args = ["Alice", "alice@example.com"]

// Multiple rows
q, args := database.Insert("users").
    Columns("name", "email").
    Values("Alice", "alice@example.com").
    Values("Bob", "bob@example.com").
    Build()
// q    = `INSERT INTO "users" ("name", "email") VALUES ($1, $2), ($3, $4)`
// args = ["Alice", "alice@example.com", "Bob", "bob@example.com"]
```

### UPDATE

```go
q, args := database.Update("users").
    Set("name = $1", "Alice").
    Set("age = $1", 30).
    Where("id = $1", 42).
    Returning("*").
    Build()
// q    = `UPDATE "users" SET name = $1, age = $2 WHERE id = $3 RETURNING *`
// args = ["Alice", 30, 42]
```

### DELETE

```go
q, args := database.Delete("users").
    Where("active = $1", false).
    Returning("id").
    Build()
// q    = `DELETE FROM "users" WHERE active = $1 RETURNING "id"`
// args = [false]
```

### Parameter Rewriting

Each `.Where()` and `.Set()` call uses `$1`-based numbering independently. The builder automatically rewrites parameters to produce correct sequential numbering in the final query:

```go
// Both Where calls use $1 locally
q.Where("age > $1", 18).Where("active = $1", true)
// Result: "... WHERE age > $1 AND active = $2"  args = [18, true]
```

## Transactions

Use `WithTx` to run a function inside a database transaction. When no
transaction boundary is already active, the transaction is committed if the
function returns `nil` and rolled back otherwise. The transaction is carried
through `context.Context`, so all queries within the function automatically
participate in it.

```go
err := database.WithTx(ctx, pool, func(ctx context.Context) error {
    // All queries here use the same transaction
    user, err := userRepo.FindByID(ctx, "id", 42)
    if err != nil {
        return err
    }

    _, err = pool.Exec(ctx, "UPDATE accounts SET balance = balance - $1 WHERE user_id = $2", amount, user.ID)
    if err != nil {
        return err // triggers rollback
    }

    return nil // triggers commit
})
```

### Nested Transactions

Nested calls to `WithTx` reuse the existing transaction rather than creating savepoints. This means all operations within a call chain share the same transaction boundary:

```go
err := database.WithTx(ctx, pool, func(ctx context.Context) error {
    // Outer transaction

    return database.WithTx(ctx, pool, func(ctx context.Context) error {
        // Reuses the outer transaction (no savepoint)
        return repo.DeleteByID(ctx, "id", 42)
    })
})
```

### Manual Transaction Access

Retrieve the current transaction from context if needed. The transaction context
is keyed per pool, so you pass the pool whose transaction you want — a
transaction begun on a different datasource pool is never returned:

```go
tx := database.TxFromContext(ctx, pool) // returns pool's pgx.Tx, or nil
```

### Request-scoped Unit of Work

`WithTx` is the function-scoped boundary. For a per-request "transaction owns the
request" boundary, opt the workload into a DI request-scoped `UnitOfWork` on the
plugin. It registers a scoped `*database.UnitOfWork` provider, and the HTTP
request-scope **commits it on handler success and rolls it back on error, panic,
or cancel** — one transaction per participating datasource pool, begun lazily on
first write:

```go
database.NewPlugin(database.PluginConfig{
    Pool:              database.PoolConfig{DSN: "postgres://..."},
    UnitOfWork:        true,
    UnitOfWorkTimeout: 5 * time.Second, // bound each boundary commit/rollback
})
```

Every repository query on a given pool within the request transparently shares
that pool's transaction — no repository changes, no explicit context threading. A
handler can inject the `*UnitOfWork` and call `SetRollbackOnly()` to force a
rollback while still returning a success response.

A UnitOfWork nested inside an outer `WithTx` for the same pool **joins** the
enclosing transaction (join-outer, no savepoints), consistent with `WithTx`
nesting above.

The reverse nesting also joins: `WithTx` called inside an active request unit
enrolls the pool in that unit and binds the same transaction into its callback
context. It does not open a second connection or finalize early, so the callback
sees the request's uncommitted rows and the request boundary still owns the
eventual commit or rollback. A callback error is returned to its caller; the
unit rolls back when that error becomes the request outcome. If an error is
handled and the request will otherwise succeed, inject the `*UnitOfWork` and
call `SetRollbackOnly()` explicitly.

`database.InUnitOfWork(ctx)` reports whether the context has an ambient
request unit. It does not enroll a pool or begin a transaction.

### Consume-once and rotation (typed Outcome)

The optimistic-concurrency helpers on `Repository[T]` return a typed, closed
`transaction.Outcome` (`go.putnami.dev/protocol/transaction`) instead of a bare
row count, so callers switch exhaustively on a stable, cross-language taxonomy
that the TypeScript adapter reports identically:

| `transaction.Outcome` | Retryable | Meaning |
|---|---|---|
| `OutcomeApplied` | no | The unit of work committed (the transition ran). |
| `OutcomeAlreadyConsumedConflict` | no | The target was already consumed (idempotency key, once-only token, or unique-violation on insert). |
| `OutcomeNotFound` | no | A required target row did not exist. |
| `OutcomeRetryableSerializationFailure` | **yes** | A serialization/deadlock abort the caller may retry (`Outcome.Retryable()` is true only here). |

**ConsumeOnce** claims a row exactly once. The still-unclaimed guard plus
Postgres row locking make it safe even under racing claims and without an
enclosing transaction — exactly one caller observes `OutcomeApplied`:

```go
// Redeem a device code exactly once.
outcome, err := codes.ConsumeOnce(ctx, "code", code,
    "consumed = false",                      // still-unclaimed guard
    "consumed = true, consumed_by = $1", uid) // claim transition + args
```

**Rotate** compare-and-sets a predecessor's state and, only when that applies,
inserts the successor. It performs two writes and is atomic **only inside a
transaction**, so wrap it in `WithTx` / a `UnitOfWork` — plus any second-table
write (e.g. the successor's binding) — for the whole revoke → install → bind
sequence to be all-or-nothing:

```go
err := database.WithTx(ctx, pool, func(ctx context.Context) error {
    outcome, err := keys.Rotate(ctx, database.RotateSpec{
        KeyColumn:        "id",
        PredecessorKey:   predecessorID,
        StateColumn:      "state",
        Expected:         "active",
        Revoked:          "revoked",
        SuccessorColumns: []string{"id", "tenant", "state"},
        SuccessorValues:  []any{successorID, tenant, "active"},
    })
    if err != nil || outcome != transaction.OutcomeApplied {
        return err // non-applied installs nothing; the empty unit is a no-op
    }
    // Second table, SAME transaction: bind the successor. A failure here rolls the
    // rotation back with it, so the successor never appears unbound.
    _, err = pool.Exec(ctx, "INSERT INTO key_bindings (key_id, tenant) VALUES ($1, $2)", successorID, tenant)
    return err
})
```

A unit spanning **several** datasources is best-effort sequential commit and
explicitly **not** two-phase commit; keep a consume-once / rotation unit within a
single datasource when the outcome must be exact. A worked, tested proof of both
primitives lives at `go/samples/unit-of-work-proof/` (with a TypeScript twin at
`typescript/samples/06-database`).

## Migrations

The Go migration runner implements the shared Putnami migration protocol. The
TypeScript and Go runtimes consume the same `migration.migrations` state store
and coordinate via a session-level Postgres advisory lock, so the two
frameworks can safely target the same database.

### Authoring Migrations

Migrations live inside the feature package that owns them, as paired SQL
files:

```
go/framework/iam/
├── plugin.go
└── migrations/
    ├── 20260520120000_create_users.up.sql
    ├── 20260520120000_create_users.down.sql   ← optional, absence = non-reversible
    └── 20260601150000_add_email_index.up.sql
```

Files are plain SQL — drag them into psql or DataGrip and they run as-is.
No `-- +goose Up`, no `StatementBegin/End`, no parser. Postgres handles
multi-statement input (including `DO $$ ... $$`) natively.

Naming: timestamp prefix + snake-case description. The lexicographic
order of the basenames is the apply order within the feature's namespace.

### Wiring a Feature Plugin

The plugin embeds its migrations FS and implements
`app.MigrationContributor`:

```go
package iam

import (
    "embed"

    "go.putnami.dev/app"
    "go.putnami.dev/database"
    "go.putnami.dev/migration"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

type Plugin struct{}

func New() *Plugin { return &Plugin{} }

func (p *Plugin) Name() string { return "iam" }

// MigrationSources is the one method you implement. The framework
// invokes it during the Migrate lifecycle phase and feeds the result
// into the per-app migration registry; the SQL runner pairs files,
// hashes them, and applies pending entries.
func (p *Plugin) MigrationSources() []migration.Source {
    return []migration.Source{
        database.NewSQLSource("iam", database.Datasource{Name: "default", Schema: "public"}, migrationsFS),
    }
}
```

A single plugin can stack multiple sources (e.g. SQL + future GCS) and
target multiple datasources. Inline definitions can be appended after
the embedded FS for rare one-off boot data:

```go
database.NewSQLSource("iam", database.Datasource{Name: "default", Schema: "public"}, migrationsFS,
    database.Definition{Name: "20260601_seed_root_role", SQL: "INSERT INTO ..."},
)
```

### Running Migrations

**On startup (auto-apply):** set `MigrationConfig.AutoApply: true` on the
database plugin. The framework's Migrate lifecycle phase runs every
runner's `Apply` after `Configure` and before HTTP starts:

```go
app.New("my-service").
    Use(database.NewPlugin(database.PluginConfig{
        Pool:      database.PoolConfig{DSN: "postgres://..."},
        Migration: &database.MigrationConfig{AutoApply: true},
    })).
    Use(iam.New()).
    ListenAndServe()
```

**Via the migrate CLI (recommended for production):** keep `AutoApply: false`
and ship a `cmd/migrate` binary in the service. It reuses
`wire.BuildApp` from `cmd/server`, so the same plugin chain — including
every feature's migrations — is picked up. Three lines:

```go
package main

import (
    "os"

    "go.putnami.dev/migratecli"

    "github.com/example/my-service/internal/wire"
)

func main() {
    os.Exit(migratecli.Run(wire.BuildApp))
}
```

Subcommands: `up`, `up to <name>`, `down`, `down to <name>`, `status`,
`verify`, `inspect`.

Each apply and rollback runs in a single Postgres transaction so the
schema change and the state-store update either both succeed or both
roll back. Down SQL is persisted to `migration.migrations.down_sql` at
apply time, so a rollback works even after the code that defined the
migration has been removed.

### Direct Migrator Use (tests)

For tests that want a single-purpose engine without going through the
plugin lifecycle, construct a `Migrator` with explicit definitions:

```go
migrator := database.NewMigrator(stdDB, database.MigrationConfig{
    Datasource:  "primary",
    Definitions: []database.Definition{
        {Name: "iam/001_create_users", SQL: "CREATE TABLE users (id TEXT PRIMARY KEY);"},
    },
})
applied, err := migrator.Up(ctx)
```

### Discoverability

- Every contribution emits a structured `database.migration` line at `debug`;
  routine registration stays out of the default boot output.
- `./migrate inspect` dumps the per-app registry as JSON (no DB access).
- `PUTNAMI_DESCRIBE=migrations` (or `=all`) writes `.gen/migrations.json`
  so CI can `jq` the registry without running the service.

A canonical worked example lives at `go/samples/migrations-feature/`.

## Error Handling

The package uses structured error codes from `go.putnami.dev/errors`. Each error is wrapped with a code indicating the failure category:

| Code | Constant | Description |
|------|----------|-------------|
| `db.connection` | `CodeConnection` | Connection pool creation or connectivity failure |
| `db.datasource` | `CodeDatasource` | A declared datasource is incomplete (missing name or schema) |
| `db.query` | `CodeQuery` | Query execution failure (find, delete, etc.) |
| `db.transaction` | `CodeTransaction` | Transaction begin, commit, or rollback failure |
| `migration.apply_failed` | `CodeMigrationApplyFailed` | A migration's up SQL or state-store insert failed |
| `migration.rollback_failed` | `CodeMigrationRollbackFailed` | A migration's down SQL or state-store delete failed |
| `migration.lock_failed` | `CodeMigrationLockFailed` | Could not acquire the advisory lock for the datasource |
| `migration.state_store_failed` | `CodeMigrationStateStore` | The `migration.migrations` table could not be created or queried |
| `migration.startup_blocked` | `CodeMigrationStartup` | Startup migration prerequisites (pool, stdlib DB) are missing |
| `migration.invalid_definition` | `CodeMigrationInvalidDef` | A registered migration is missing required fields |

Errors are wrapped with context using `errors.Wrapf`, preserving the original error for inspection:

```go
user, err := repo.FindByID(ctx, "id", 42)
if err != nil {
    // err contains the code "db.query" and the original pgx error
    var coded *errors.Error
    if errors.As(err, &coded) {
        fmt.Println(coded.Code) // "db.query"
    }
}
```

## Best Practices

1. **Use the plugin for lifecycle management.** The `database.Plugin` handles pool creation, DI registration, and graceful shutdown. Avoid creating pools manually unless you have a specific need.

2. **Always use `WithTx` for multi-statement operations.** Never begin transactions manually -- `WithTx` ensures proper commit/rollback and propagates the transaction through context.

3. **Write scan functions carefully.** The `scanRow` function must match the column order of `SELECT *` for your table. If you add columns to a table, update the scan function.

4. **Use the query builder for dynamic queries.** It handles parameter renumbering automatically and produces parameterized queries that prevent SQL injection.

5. **Keep repositories thin.** Embed `*database.Repository[T]` and add only domain-specific finder and mutation methods. Delegate complex queries to the query builder.

6. **Write migration rollbacks.** Always include a `Down` field on every `database.Definition` so migrations can be reversed during development and incident recovery. The down SQL is persisted with the state-store row, so rollbacks remain available after the defining code is removed.

7. **Configure pool sizing for your workload.** The defaults (10 max, 2 min) work for small services. For high-throughput services, increase `MaxConns` and monitor with `pool.Stats()`.

8. **Query through the pool in shared code.** `pool.Exec`, `pool.Query` and `pool.QueryRow` join the active `WithTx` transaction or the request's unit of work, making your code transparent to transaction boundaries. `pool.Querier(ctx)` sees only a `WithTx` transaction.
