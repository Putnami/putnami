# Persistence

`go.putnami.dev/database` provides PostgreSQL integration with connection pooling (via pgx/v5), transactions, a generic repository pattern, schema migrations (cross-language Putnami migration protocol), and a query builder.

It is a **stable** public package owned by the Go SDD surface. Transaction
atomicity is pool-scoped: a unit of work can coordinate several datasource
transactions, but does not claim distributed all-or-nothing commit. See the
persistence and transaction specification and accepted pool-scoped atomicity
decision record next to the package source.

## Connection pool

### Creating a pool

```go
import "go.putnami.dev/database"

pool, err := database.NewPool(ctx, database.PoolConfig{
    DSN:      "postgres://user:pass@localhost:5432/mydb?sslmode=disable",
    MaxConns: 10,
    MinConns: 2,
})
if err != nil {
    log.Fatal(err)
}
defer pool.Close()
```

### Pool configuration

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `DSN` | `string` | — | PostgreSQL connection string |
| `MaxConns` | `int32` | `10` | Maximum connections |
| `MinConns` | `int32` | `2` | Minimum idle connections |
| `MaxConnLifetime` | `time.Duration` | `1h` | Maximum connection lifetime |
| `MaxConnIdleTime` | `time.Duration` | `30m` | Maximum idle time |
| `HealthCheckPeriod` | `time.Duration` | `1m` | Health check interval |

### Direct queries

```go
// Single row
row := pool.QueryRow(ctx, "SELECT name FROM users WHERE id = $1", userID)

// Multiple rows
rows, err := pool.Query(ctx, "SELECT * FROM users WHERE active = $1", true)

// Execute (INSERT, UPDATE, DELETE)
tag, err := pool.Exec(ctx, "DELETE FROM sessions WHERE expires_at < $1", time.Now())

// Health check
err := pool.Ping(ctx)

// Pool statistics
stats := pool.Stats()
```

## Repository pattern

The generic `Repository[T]` provides typed CRUD operations for a database table:

### Defining a repository

The finder methods (`FindByID`, `FindAll`, `FindWhere` and the others) select
`*`, so the scan function receives every column of the table, in the order the
table declares them. Scan one destination per column, and make a raw `Query` or
`QueryOne` return the same columns. The examples on this page use this table:

```sql
CREATE TABLE users (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    email      TEXT NOT NULL,
    age        INT NOT NULL,
    active     BOOLEAN NOT NULL DEFAULT true,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

```go
import (
    "time"

    "github.com/jackc/pgx/v5"
    "go.putnami.dev/database"
)

type User struct {
    ID        string
    Name      string
    Email     string
    Age       int
    Active    bool
    CreatedAt time.Time
}

func scanUser(row pgx.Row) (User, error) {
    var u User
    err := row.Scan(&u.ID, &u.Name, &u.Email, &u.Age, &u.Active, &u.CreatedAt)
    return u, err
}

type UserRepository struct {
    *database.Repository[User]
}

func NewUserRepository(pool *database.Pool) *UserRepository {
    return &UserRepository{
        Repository: database.NewRepository[User](pool, "users", scanUser),
    }
}
```

### Repository methods

```go
repo := NewUserRepository(pool)

// Find by primary key
user, err := repo.FindByID(ctx, "id", "user-123")

// Find all
users, err := repo.FindAll(ctx)

// Find with WHERE clause
users, err := repo.FindWhere(ctx, "age > $1 AND active = $2", 18, true)

// Find one with WHERE clause
user, err := repo.FindOneWhere(ctx, "email = $1", "jane@example.com")

// Count
count, err := repo.Count(ctx, "active = $1", true)

// Exists
exists, err := repo.Exists(ctx, "email = $1", "jane@example.com")

// Delete
deleted, err := repo.DeleteWhere(ctx, "active = $1", false)
err = repo.DeleteByID(ctx, "id", "user-123")

// Raw queries
users, err := repo.Query(ctx, "SELECT * FROM users ORDER BY created_at DESC LIMIT $1", 10)
user, err := repo.QueryOne(ctx, "SELECT * FROM users WHERE email = $1", "jane@example.com")
```

### Custom repository methods

Extend the repository with domain-specific queries:

```go
func (r *UserRepository) FindByEmail(ctx context.Context, email string) (User, error) {
    return r.FindOneWhere(ctx, "email = $1", email)
}

func (r *UserRepository) FindActive(ctx context.Context) ([]User, error) {
    return r.FindWhere(ctx, "active = true")
}

func (r *UserRepository) Create(ctx context.Context, user User) error {
    _, err := r.Pool().Exec(ctx,
        "INSERT INTO users (id, name, email, age) VALUES ($1, $2, $3, $4)",
        user.ID, user.Name, user.Email, user.Age,
    )
    return err
}
```

## Transactions

### Basic transaction

```go
err := database.WithTx(ctx, pool, func(ctx context.Context) error {
    // All queries within this function use the same transaction
    _, err := pool.Exec(ctx,
        "INSERT INTO users (id, name, email, age) VALUES ($1, $2, $3, $4)",
        id, name, email, age,
    )
    if err != nil {
        return err // triggers rollback
    }

    _, err = pool.Exec(ctx, "INSERT INTO audit_log (user_id, action) VALUES ($1, $2)", id, "created")
    return err // nil = commit, error = rollback
})
```

### Nested transactions

Nested calls to `WithTx` reuse the existing transaction:

```go
err := database.WithTx(ctx, pool, func(ctx context.Context) error {
    repo.Create(ctx, user)

    // This is still the same transaction
    return database.WithTx(ctx, pool, func(ctx context.Context) error {
        return repo.CreateAuditLog(ctx, user.ID, "created")
    })
})
```

Nested reuse applies only to the same pool. A transaction from another pool is
not silently reused; multi-datasource work has no two-phase-commit guarantee.

### Request-scoped unit of work

Set `UnitOfWork` on the plugin to make every HTTP request transactional without
calling `WithTx`:

```go
a.Use(database.NewPlugin(database.PluginConfig{
    Datasource: "default",
    UnitOfWork: true,
}))
```

Within a request, the first query on a pool begins that pool's transaction, and
later queries on the same pool join it. A request that sends no query opens no
transaction. The request boundary commits or rolls back by outcome, as the
[HTTP guide](/docs/frameworks/go/http) lists. To roll back a request that still
returns a success response, mark its unit of work:

```go
uow, err := inject.Resolve[*database.UnitOfWork](ctx, inject.TokenOf[*database.UnitOfWork]())
if err != nil {
    return err
}
uow.SetRollbackOnly()
```

A `WithTx` inside the request joins the unit's transaction and leaves the commit
to the boundary. A unit that spans several pools commits them one after the
other, not atomically. `UnitOfWorkTimeout` bounds each commit and rollback.

### Transaction context

`Exec`, `Query` and `QueryRow` on the pool, and every repository method, join
the active `WithTx` transaction or the request's unit of work. The pool's
`Querier` method detects only a `WithTx` transaction: inside a unit of work, it
returns the pool and its queries autocommit.

```go
// Outside a transaction: uses pool connection
q := pool.Querier(ctx)

// Inside WithTx: uses the transaction
database.WithTx(ctx, pool, func(ctx context.Context) error {
    q := pool.Querier(ctx) // uses the transaction connection
    q.Exec(ctx, "...")
    return nil
})
```

You can also check for an active transaction:

```go
// The WithTx transaction on this pool; nil inside a unit of work alone.
// database.InUnitOfWork(ctx) reports a request-scoped unit of work.
tx := database.TxFromContext(ctx, pool)
if tx != nil {
    // inside a transaction
}
```

## Query builder

A lightweight builder for common SQL patterns. `Build` quotes the table and the
columns you name, never a condition. Number the placeholders of each `Where`,
`Set` or `Having` call from `$1`: the builder shifts them past the arguments
earlier calls added.

### SELECT

```go
import "go.putnami.dev/database"

query, args := database.Select("users").
    Columns("id", "name", "email").
    Where("age > $1", 18).
    Where("active = $1", true).
    OrderBy("name ASC").
    Limit(10).
    Offset(20).
    Build()

// query: SELECT "id", "name", "email" FROM "users" WHERE age > $1 AND active = $2 ORDER BY "name" ASC LIMIT 10 OFFSET 20
// args: [18, true]
```

### INSERT

```go
query, args := database.Insert("users").
    Columns("id", "name", "email", "age").
    Values("user-123", "Jane", "jane@example.com", 34).
    Returning("id", "created_at").
    Build()

// query: INSERT INTO "users" ("id", "name", "email", "age") VALUES ($1, $2, $3, $4) RETURNING "id", "created_at"
```

### UPDATE

```go
query, args := database.Update("users").
    Set("name = $1", "Jane Doe").
    Set("age = $1", 35).
    Where("id = $1", "user-123").
    Build()

// query: UPDATE "users" SET name = $1, age = $2 WHERE id = $3
```

### DELETE

```go
query, args := database.Delete("users").
    Where("active = $1", false).
    Returning("id").
    Build()

// query: DELETE FROM "users" WHERE active = $1 RETURNING "id"
```

## Plugin

The SQL plugin integrates the connection pool with the application lifecycle:

```go
import (
    "go.putnami.dev/app"
    "go.putnami.dev/database"
)

a := app.New("my-service")
a.Use(database.NewPlugin(database.PluginConfig{
    Datasource: "default",
    Pool:       database.PoolConfig{DSN: cfg.DatabaseURL},
    Migration:  &database.MigrationConfig{AutoApply: true},
}))
```

The plugin:
- Registers `*database.Pool` (and the `*database.Pools` registry) in the DI container
- Opens the pool when the application starts
- Applies the contributed migrations before the server listens, when `Migration.AutoApply` is true
- Closes the pool on application shutdown

### Get the pool in a handler

Let the container hand the pool to the code that queries. `http.Inject` resolves
each parameter of a handler before it runs:

```go
server.GET("/users/{id}", http.Inject(func(pool *database.Pool, ctx *http.Context) *http.Response {
    var name string
    err := pool.QueryRow(ctx.Context(), "SELECT name FROM users WHERE id = $1", ctx.Param("id")).Scan(&name)
    if err != nil {
        return http.ErrorResponse(err)
    }
    return http.JSON(map[string]string{"name": name})
}))
```

A provider factory that resolves the pool works the same way: the container
calls it after the pool exists. See [Dependency Injection](/docs/frameworks/go/dependency-injection).

Do not resolve the pool, open a pool, or ping the database in `main`. The
build runs `main` to describe the application, with no database, and the
build fails. See
[Plugins & Lifecycle](/docs/frameworks/go/plugins-and-lifecycle#main-runs-at-build-time).

## Connect at run time

When `PluginConfig.Datasource` names a datasource, the plugin takes its
connection from the first of these that is present:

1. The `database` section of the application config, or the
   `DATABASE_BINDINGS` environment variable. A deploy target injects one of
   them: a binding document keyed by datasource name.
2. `PluginConfig.Pool`: the `DSN` (or `Connection`) your code passes.

Without `Datasource`, the plugin uses `PluginConfig.Pool` as it is and reads
no binding. Name the datasource so the same binary runs locally with a DSN
and in a deploy target with a binding.

The framework reads no `DATABASE_URL` variable. To run the service against a
plain Postgres URL, read it from your configuration and pass it as the `DSN`:

```go
type ServerConfig struct {
    DatabaseURL string `json:"databaseUrl" env:"DATABASE_URL"`
}
```

```bash
DATABASE_URL='postgres://user:pass@127.0.0.1:5432/app?sslmode=disable' ./api
```

A binding document for the default datasource looks like this:

```json
{
  "protocolVersion": 1,
  "databases": {
    "default": { "engine": "postgres", "schema": "public", "connection": { "dsn": "postgres://user:pass@127.0.0.1:5432/app" } }
  }
}
```

## Migrations

A migration is a pair of plain SQL files. A plugin embeds them and contributes
them by implementing `app.MigrationContributor`:

```text
api/migrations/
├── 20260520120000_create_users.up.sql
└── 20260520120000_create_users.down.sql   # optional; without it the migration is not reversible
```

```go
import (
    "embed"

    "go.putnami.dev/database"
    "go.putnami.dev/migration"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

type usersPlugin struct{}

func (usersPlugin) Name() string { return "users" }

func (usersPlugin) MigrationSources() []migration.Source {
    return []migration.Source{
        database.NewSQLSource("users", database.Datasource{Name: "default", Schema: "public"}, migrationsFS),
    }
}
```

Register the plugin with `a.Use(usersPlugin{})`. Migrations run in name order.
With `Migration.AutoApply`, the application applies pending migrations at
startup, before the server listens. Without it, run them with a migration
command; see [Migrations](/docs/frameworks/go/migrations).

The Go and TypeScript runners share the `migration.migrations` state table and
one Postgres advisory lock per datasource, so both languages can target the
same database. Each migration runs in its own transaction, and its down SQL is
stored with it, so a rollback works after the code that registered it is gone.

To test against the database `putnami test` provides, start the application
with `AutoApply`; see
[Test against Postgres](/docs/frameworks/go/testing#test-against-postgres).

### Run migrations from code

`database.NewMigrator` drives one datasource directly:

```go
migrator := database.NewMigrator(stdDB, database.MigrationConfig{
    Definitions: definitions,
})

applied, err := migrator.Up(ctx)                       // apply pending migrations
last, err := migrator.Rollback(ctx)                    // roll back the latest one
rolledBack, err := migrator.RollbackTo(ctx, "users/20260520120000_create_users")
status, err := migrator.Status(ctx)                    // every recorded migration
```

## Error codes

| Code | Description |
|------|-------------|
| `db.connection` | Connection pool creation failed |
| `db.query` | Query execution failed |
| `migration.apply_failed` | A migration's up SQL or state-store insert failed |
| `migration.rollback_failed` | A migration's down SQL or state-store delete failed |
| `migration.lock_failed` | Could not acquire the per-datasource advisory lock |
| `migration.state_store_failed` | The `migration.migrations` table could not be created or queried |
| `migration.startup_blocked` | Startup migration prerequisites are missing |
| `migration.invalid_definition` | A registered migration is missing required fields |
| `db.transaction` | Transaction begin/commit/rollback failed |

## Related guides

- [Dependency Injection](/docs/frameworks/go/dependency-injection) — injecting the pool
- [Plugins & Lifecycle](/docs/frameworks/go/plugins-and-lifecycle) — SQL plugin lifecycle
- [Migrations](/docs/frameworks/go/migrations) — registry and CLI execution
- [Errors](/docs/frameworks/go/errors) — structured error handling
