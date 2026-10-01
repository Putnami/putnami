# @putnami/database

SQL database integration with PostgreSQL support, migrations, and repository pattern.

## Features

- 🗄️ **PostgreSQL Integration** - Full PostgreSQL support with one connection pool per database, shared by every datasource (schema) of that database
- 🏗️ **Fluent Schema API** - Declarative table definitions with `Table`, `Column`, and `Key` builders
- 📦 **Repository Pattern** - Type-safe database operations with a clean API
- 🔄 **Migrations with Rollback** - Schema versioning with automatic execution and reversible migrations
- 🔌 **Multiple Datasources** - Support for multiple database connections
- 💾 **Session Store** - Database-backed session storage
- 🔒 **Type Safety** - Full TypeScript support with `InferTable` for compile-time type checking
- 🔍 **Advanced Query Builder** - Support for operators (IN, LIKE, >, <, etc.) and complex filters ($or, $and)
- 📊 **Bulk Operations** - Efficient batch insert/update/delete operations
- 📄 **Pagination Support** - Built-in offset/limit for paginated queries
- 📈 **Query Profiling** - Optional query performance monitoring
- ⏱️ **Transaction Timeout** - Auto-rollback transactions that exceed a deadline
- 🛑 **Request Abort** - Cancel in-flight queries and release connections on client disconnect

## Installation

```bash
bun add @putnami/database
```

## Quick Start

### 1. Configure Database

Create `.env.local.yaml`:

```yaml
database:
  host: localhost
  port: 5432
  database: myapp
  user: postgres
  password: your_password
```

#### Cloud SQL on GCP

The same configuration works on Cloud Run and on a dev laptop, with no
password in config:

- **Cloud Run**: deploy with `--add-cloudsql-instances=<instance>`. GCP
  mounts a Unix socket at `/cloudsql/<instance>/.s.PGSQL.5432`. Set
  `host` to that socket directory:

  ```yaml
  database:
    default:
      host: /cloudsql/my-project:europe-west1:my-instance
      database: auth
  ```

  When `host` starts with `/`, the framework treats it as a Unix socket,
  fetches an IAM access token from the metadata server per connection, and
  uses it as the Postgres password.

- **Dev laptop**: run [`cloud-sql-proxy`](https://cloud.google.com/sql/docs/postgres/connect-auth-proxy)
  on a TCP port, then point at it:

  ```yaml
  database:
    default:
      host: localhost
      port: 6543
      database: auth
  ```

  The proxy authenticates outbound with your `gcloud auth application-default
  login` credentials; no password is needed in config.

When `user` is omitted, the framework resolves the active GCP IAM principal
at startup — the metadata server's service-account email on Cloud Run /
GCE / Cloud Build, or `gcloud config get-value account` on a dev laptop.
The `.gserviceaccount.com` suffix is stripped automatically.

### 2. Define a Table

```typescript
import { Table, Column, Key, Uuid, Email } from "@putnami/database";
import type { InferTable } from "@putnami/database";

const CreateUsersTable = {
  name: "20250101000000-create-users-table",
  sql: `
    CREATE TABLE IF NOT EXISTS users (
      id UUID PRIMARY KEY,
      email VARCHAR(255) UNIQUE NOT NULL,
      name VARCHAR(255) NOT NULL
    );
  `,
};

const UsersTable = Table("users", {
  id:    Key(Uuid),
  email: Column(Email),
  name:  Column(String),
}, {
  migrations: [CreateUsersTable],
});

type User = InferTable<typeof UsersTable>;
```

### 3. Create a Repository

```typescript
import { Repository } from "@putnami/database";

class UserRepository extends Repository<typeof UsersTable> {
  constructor() {
    super(UsersTable);
  }
}
```

### 4. Use the Repository

```typescript
const userRepo = new UserRepository();

// Create
const user = await userRepo.save({
  id: "550e8400-e29b-41d4-a716-446655440000",
  email: "john@example.com",
  name: "John Doe",
});

// Read
const found = await userRepo.get({ id: "550e8400-e29b-41d4-a716-446655440000" });

// Update
const updated = await userRepo.save({
  id: "550e8400-e29b-41d4-a716-446655440000",
  name: "John Smith",
});

// Delete
await userRepo.delete({ id: "550e8400-e29b-41d4-a716-446655440000" });
```

### 5. Enable Automatic Migrations

```typescript
import { application, http, api, platform } from "@putnami/application";
import { sql } from "@putnami/database";

export const app = () =>
  application()
    .use(http())
    .use(sql())     // Automatically runs migrations on startup
    .use(platform())
    .use(api());
```

**That's it!** Your migrations will run automatically when the application starts.

## Documentation

Comprehensive documentation is available in the [`doc/`](./doc/) directory:

- **[Quick Start Guide](./doc/quick-start.md)** - Get up and running in minutes
- **[Table Definitions](./doc/entities.md)** - Define database tables with the fluent schema API
- **[Repository Pattern](./doc/repository.md)** - Type-safe database operations
- **[Migrations](./doc/migrations.md)** - Schema versioning and management
- **[Transactions](./doc/transactions.md)** - Atomic operations, timeouts, and abort handling
- **[Advanced Queries](./doc/advanced-queries.md)** - JOINs, subqueries, CTEs, and aggregations
- **[Database Connections](./doc/connections.md)** - Configure and manage connections
- **[Observability](./doc/observability.md)** - Metrics, slow query detection, and structured logging
- **[Session Store](./doc/session-store.md)** - Database-backed session storage

## Core Concepts

### Fluent Schema API

Define database tables using `Table`, `Column`, and `Key` builders with schema primitives:

```typescript
import { Table, Column, Key, Uuid, Email, Optional } from "@putnami/database";
import type { InferTable } from "@putnami/database";

const UsersTable = Table("users", {
  id:    Key(Uuid, { autoGenerate: true }),
  email: Column(Email),
  name:  Column(String, { columnName: "full_name" }),
  bio:   Column(Optional(String)),
});

type User = InferTable<typeof UsersTable>;
```

**Learn more:** [Table Definitions Documentation](./doc/entities.md)

### Repository Pattern

Extend the `Repository` class for type-safe database operations:

```typescript
class UserRepository extends Repository<typeof UsersTable> {
  constructor() {
    super(UsersTable);
  }

  async findByEmail(email: string): Promise<User | undefined> {
    return this.findOne({ email });
  }
}
```

**Available methods:**
- `get(keys)` - Get by primary key
- `findOne(filters)` - Find single entity
- `find(filters, options)` - Find multiple entities with pagination
- `save(entity, options?)` - Insert or update (use `{ strict: true }` to validate all required fields)
- `saveMany(entities)` - Bulk insert/update
- `delete(keys)` - Delete entity
- `deleteMany(filters)` - Bulk delete
- `exists(keys)` - Check existence

**Learn more:** [Repository Pattern Documentation](./doc/repository.md)

### Migrations

Define schema changes as migrations and attach them to table definitions:

```typescript
const CreateUsersTable = {
  name: "20250101000000-create-users-table",
  sql: `CREATE TABLE users (...)`,
};

const UsersTable = Table("users", {
  id:    Key(Uuid),
  email: Column(Email),
}, {
  migrations: [CreateUsersTable],
});
```

Migrations run automatically on application startup when using the SQL plugin.

**Learn more:** [Migrations Documentation](./doc/migrations.md)

### Database Connections

Get database connections with automatic pooling:

```typescript
import { database } from "@putnami/database";

// Default connection
const sql = await database();

// Named datasource
const authDb = await database("auth");

// Raw SQL queries
const users = await sql`SELECT * FROM users WHERE status = ${"active"}`;
```

**Learn more:** [Database Connections Documentation](./doc/connections.md)

### Session Store

Use PostgreSQL for session storage:

```yaml
session:
  store: "database"
  ttl: 604800  # 1 week
```

**Learn more:** [Session Store Documentation](./doc/session-store.md)

## Examples

### Complete Example

```typescript
import { Table, Column, Key, Uuid, Email, DateIso, Repository } from "@putnami/database";
import type { InferTable } from "@putnami/database";

// Define table
const CreateUsersTable = {
  name: "20250101000000-create-users-table",
  sql: `
    CREATE TABLE IF NOT EXISTS users (
      id UUID PRIMARY KEY,
      email VARCHAR(255) UNIQUE NOT NULL,
      name VARCHAR(255) NOT NULL,
      created_at TIMESTAMPTZ DEFAULT NOW()
    );
  `,
};

const UsersTable = Table("users", {
  id:        Key(Uuid),
  email:     Column(Email),
  name:      Column(String),
  createdAt: Column(DateIso, { columnName: "created_at" }),
}, {
  migrations: [CreateUsersTable],
});

type User = InferTable<typeof UsersTable>;

// Create repository
class UserRepository extends Repository<typeof UsersTable> {
  constructor() {
    super(UsersTable);
  }

  async findByEmail(email: string): Promise<User | undefined> {
    return this.findOne({ email });
  }
}

// Use repository
const userRepo = new UserRepository();

// Create user
const user = await userRepo.save({
  id: "550e8400-e29b-41d4-a716-446655440000",
  email: "john@example.com",
  name: "John Doe",
});

// Find user
const found = await userRepo.findByEmail("john@example.com");
```

### Multiple Datasources

```yaml
database:
  host: localhost
  database: main_db

  auth:
    host: auth-db.example.com
    database: auth_db

  analytics:
    host: analytics-db.example.com
    database: analytics_db
```

```typescript
const UsersTable = Table("users", {
  id:    Key(Uuid),
  email: Column(Email),
}, {
  db: "auth", // Uses database.auth config
});
```

### Raw SQL Queries

```typescript
import { database } from "@putnami/database";

const sql = await database();

// Complex query
const results = await sql`
  SELECT
    u.*,
    COUNT(o.id) as order_count
  FROM users u
  LEFT JOIN orders o ON o.user_id = u.id
  WHERE u.status = ${"active"}
  GROUP BY u.id
  ORDER BY order_count DESC
  LIMIT ${10}
`;

// Transactions
await sql.begin(async (sql) => {
  await sql`INSERT INTO users (id, email) VALUES (${id}, ${email})`;
  await sql`INSERT INTO profiles (user_id, name) VALUES (${id}, ${name})`;
});
```

## Configuration

### PostgreSQL Configuration

```yaml
database:
  host: localhost
  port: 5432
  database: myapp
  user: postgres
  password: your_password
  ssl: false
  poolSize: 10
  debug: false
```

### Session Store Configuration

```yaml
session:
  store: "database"
  ttl: 604800  # 1 week in seconds
```

## API Reference

### Exports

- `Table` - Table definition builder
- `Column` - Column definition builder
- `Key` - Primary key column builder
- `InferTable` - Type utility to infer entity type from a table definition
- `Repository<TABLE>` - Base repository class
- `database(name?, config?)` - Connection factory (pool-level)
- `useTxConnection(dbName, mode)` - Abort-aware, transaction-aware connection getter
- `closeDatabase(name?)` - Close a specific database connection
- `closeAllDatabases()` - Close all database connections
- `runInTransaction(fn, options?)` - Run a function in a transaction with auto commit/rollback
- `withTransaction(options?)` - Flag current context for transactional mode
- `commit()` / `rollback()` - Manual transaction control
- `TransactionMiddleware()` - HTTP middleware for transaction cleanup and abort handling
- `TransactionOptions` - Options type (`{ timeoutMs?: number }`)
- `abortableQuery(pending)` - Wrap a postgres.js query with abort signal cancellation
- `throwIfAborted()` - Throw `QueryAbortError` if the context signal is aborted
- `useAbortSignal()` - Read the `AbortSignal` from the current context
- `QueryAbortError` - Error thrown on query abort (code `57014`)
- `PostgresConfig` - Configuration class
- `RepositoryError` - Custom error type for repository operations
- `MigrationError` - Custom error type for migration operations
- `QueryFilters<ENTITY>` - Type for advanced query filters with operators
- `MigrationService` - Migration execution service (`runMigrations`, `rollback`, `rollbackTo`)
- `migrationRegistry` - Migration registry
- `DatabaseSessionStore` - Session store implementation
- `sql()` - SQL plugin for automatic migrations

### Schema Primitives (re-exported from `@putnami/runtime`)

- `String` - Text type
- `Number` - Numeric type
- `Int` - Integer type
- `Boolean` - Boolean type
- `Uuid` - UUID type
- `Email` - Email string type
- `DateIso` - ISO date string type
- `Optional(Type)` - Nullable wrapper

## Requirements

- **Bun** v1.4.0 or higher
- **PostgreSQL** 12 or higher
- **TypeScript** 5.0 or higher (no decorator configuration required)

## Maintained contract

This package is `stable` in the workspace
[support catalog](../../../putnami.support.json). It carries two public
promises:

- **[Relational persistence](specs/relational-persistence.json)**, with the
  [request-is-the-transaction-boundary ADR](doc/adr/0001-the-request-is-the-transaction-boundary.md).
  Transactional mode reserves nothing until the first write; the request-scoped
  `UnitOfWork` is the sole committer and finalizes at most once; a unit spanning
  several datasources is best-effort and reports a partial commit as an error,
  never as success; and a rollback cause is a classified code, never an error
  message, a bound parameter, or row data.
- **[SQL migrations](specs/sql-migrations.json)**, with the
  [order-is-the-name ADR](doc/adr/0002-migration-order-is-the-name-not-the-clock.md).
  Migrations apply in byte order of their name — identical to the Go runner that
  shares the state store — roll back in the exact reverse of that order, commit
  their body and their bookkeeping row together, and refuse to run when an
  applied migration's committed SQL no longer hashes to what was applied.

Before v1.0.0, follow the workspace
[compatibility policy](../../../RELEASE.md): a breaking change is allowed in a
minor `0.x` release when the migration is documented. Do not infer strict
compatibility between every `0.x` minor.

## License

[FSL-1.1-MIT](../../../LICENSE.md)
