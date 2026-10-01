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

```go
import (
    "github.com/jackc/pgx/v5"
    "go.putnami.dev/database"
)

type User struct {
    ID    string
    Name  string
    Email string
    Age   int
}

func scanUser(row pgx.Row) (User, error) {
    var u User
    err := row.Scan(&u.ID, &u.Name, &u.Email, &u.Age)
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
    q := r.Pool().Querier(ctx)
    _, err := q.Exec(ctx,
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

### Transaction context

The pool's `Querier` method automatically detects transactions from the context:

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
tx := database.TxFromContext(ctx)
if tx != nil {
    // inside a transaction
}
```

## Query builder

A lightweight builder for common SQL patterns:

### SELECT

```go
import "go.putnami.dev/database"

query, args := database.Select("users").
    Columns("id", "name", "email").
    Where("age > $1", 18).
    Where("active = $2", true).
    OrderBy("name ASC").
    Limit(10).
    Offset(20).
    Build()

// query: "SELECT id, name, email FROM users WHERE age > $1 AND active = $2 ORDER BY name ASC LIMIT 10 OFFSET 20"
// args: [18, true]
```

### INSERT

```go
query, args := database.Insert("users").
    Columns("id", "name", "email").
    Values("user-123", "Jane", "jane@example.com").
    Returning("id", "created_at").
    Build()
```

### UPDATE

```go
query, args := database.Update("users").
    Set("name = $1", "Jane Doe").
    Set("updated_at = $2", time.Now()).
    Where("id = $3", "user-123").
    Build()
```

### DELETE

```go
query, args := database.Delete("users").
    Where("active = $1", false).
    Returning("id").
    Build()
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
