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

Every repository method selects `*`, so the scan function receives every column
of the table, in the order the table declares them. Scan one destination per
column. The examples on this page use this table:

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
    _, err := pool.Exec(ctx, "INSERT INTO users (id, name) VALUES ($1, $2)", id, name)
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
tx := database.TxFromContext(ctx, pool) // the WithTx transaction on this pool
if tx != nil {
    // inside a transaction
}
```

## Query builder

A lightweight builder for common SQL patterns. `Build` quotes the table and the
columns you name, never a condition. Number the placeholders of each `Where`, `Set` or `Having` call from
`$1`: the builder shifts them past the arguments earlier calls added.

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
    Columns("id", "name", "email").
    Values("user-123", "Jane", "jane@example.com").
    Returning("id", "created_at").
    Build()

// query: INSERT INTO "users" ("id", "name", "email") VALUES ($1, $2, $3) RETURNING "id", "created_at"
```

### UPDATE

```go
query, args := database.Update("users").
    Set("name = $1", "Jane Doe").
    Set("updated_at = $1", time.Now()).
    Where("id = $1", "user-123").
    Build()

// query: UPDATE "users" SET name = $1, updated_at = $2 WHERE id = $3
```

### DELETE

```go
query, args := database.Delete("users").
    Where("active = $1", false).
    Returning("id").
    Build()

// query: DELETE FROM "users" WHERE active = $1 RETURNING "id"
```

## Migrations

Migrations are Go values registered against a `*database.Registry`. The Go
and TypeScript runners share the canonical `migration.migrations` state store
and a per-datasource Postgres advisory lock, so both languages can target the
same database safely.

```go
import "go.putnami.dev/database"

// Register migrations (typically in package init)
_ = database.DefaultRegistry.Register("default", database.Definition{
    Name: "20260512120000-create-users",
    SQL:  "CREATE TABLE users (id TEXT PRIMARY KEY, name TEXT NOT NULL);",
    Down: "DROP TABLE users;",
})

migrator := database.NewMigrator(stdDB, database.MigrationConfig{})

// Run pending migrations for this datasource
applied, err := migrator.Up(ctx)

// Roll back the most recent migration
last, err := migrator.Rollback(ctx)

// Roll back every migration applied after the named target
rolledBack, err := migrator.RollbackTo(ctx, "20260512120000-create-users")

// Read every recorded migration row
status, err := migrator.Status(ctx)

// Roll back every applied migration
all, err := migrator.Reset(ctx)
```

Each operation runs inside one transaction per migration, and `down_sql` is
persisted alongside its hash so rollbacks remain available after the code
that registered the migration is removed.

## Plugin

The SQL plugin integrates the connection pool with the application lifecycle:

```go
import (
    "go.putnami.dev/app"
    "go.putnami.dev/database"
)

a := app.New("my-service")
a.Module.Use(database.NewPlugin(database.PluginConfig{
    DSN:      "postgres://localhost/mydb",
    MaxConns: 10,
}))
```

The plugin:
- Creates the connection pool during warmup
- Registers `*database.Pool` in the DI container
- Closes the pool on application shutdown

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
