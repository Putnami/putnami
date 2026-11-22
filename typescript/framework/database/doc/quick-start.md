# Quick Start Guide

Get up and running with `@putnami/database` in minutes. This guide will walk you through setting up a database connection, defining tables, and performing basic operations.

## Installation

```bash
bun add @putnami/database
```

## Step 1: Configure Database Connection

Give the connection as one Postgres URL, in the `DATABASE_URL` environment
variable or under the datasource's `url` key. Each datasource has its own key
under `database`; `default` is the one `database()` and repositories use when
you name none. In `conf/.env.local.yaml`:

```yaml
database:
  default:
    url: postgres://postgres:your_password@localhost:5432/myapp
```

Every other datasource gets its own URL: `DATABASE_URL` configures only the
primary datasource (`default`, unless `sql({ datasource })` names another):

```yaml
database:
  default:
    url: postgres://postgres:your_password@localhost:5432/myapp

  # Named datasources
  auth:
    url: postgres://auth_user:auth_password@auth-db.example.com:5432/auth_db
```

See [Connections](./connections.md) for the structured `host`/`port`/`database`
keys and the order in which bindings, URLs and keys apply.

## Step 2: Define Your First Table

Create a table definition using the fluent `Table`, `Key`, and `Column` builders:

```typescript
import { Table, Column, Key, Uuid, Email, Optional, DateIso } from "@putnami/database";
import type { InferTable } from "@putnami/database";

const UsersTable = Table("users", {
  id:        Key(Uuid),
  email:     Column(Email),
  name:      Column(String),
  bio:       Column(Optional(String)),
  status:    Column(String),
  createdAt: Column(DateIso, { columnName: "created_at" }),
});

type User = InferTable<typeof UsersTable>;
```

## Step 3: Create a Migration

A migration is a named SQL script. Migrations are registered with the SQL
plugin (Step 6) — they are **not** attached to the table definition:

```typescript
const CreateUsersTable = {
  name: "20250101000000-create-users-table",
  sql: `
    CREATE TABLE IF NOT EXISTS users (
      id UUID PRIMARY KEY,
      email VARCHAR(255) UNIQUE NOT NULL,
      name VARCHAR(255) NOT NULL,
      bio TEXT,
      status VARCHAR(32) NOT NULL DEFAULT 'active',
      created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
    );
  `,
};
```

> To keep the SQL in `.sql` files, import each file's text with
> `with { type: 'text' }`; the build generates nothing for you. See [Migrations](./migrations.md).

## Step 4: Create a Repository

Extend the `Repository` class for type-safe database operations:

```typescript
import { Repository } from "@putnami/database";

class UserRepository extends Repository<typeof UsersTable> {
  constructor() {
    super(UsersTable);
  }

  // Add custom methods as needed
  async findByEmail(email: string): Promise<User | undefined> {
    return this.findOne({ email });
  }
}
```

## Step 5: Use the Repository

Now you can perform database operations:

```typescript
const userRepo = new UserRepository();

// Create a user
const newUser = await userRepo.save({
  id: "550e8400-e29b-41d4-a716-446655440000",
  email: "john@example.com",
  name: "John Doe",
  bio: "Software developer",
});

// Find a user
const user = await userRepo.get({ id: "550e8400-e29b-41d4-a716-446655440000" });

// Find by email
const userByEmail = await userRepo.findByEmail("john@example.com");

// Find multiple users
const activeUsers = await userRepo.find(
  { status: "active" },
  { limit: 10, orderBy: "created_at DESC" }
);

// Update a user
const updated = await userRepo.save({
  id: "550e8400-e29b-41d4-a716-446655440000",
  name: "John Smith", // Updated name
});

// Delete a user
const { success } = await userRepo.delete({ id: "550e8400-e29b-41d4-a716-446655440000" });
```

## Step 6: Enable Automatic Migrations

Contribute the migration from a plugin via `sqlSourceInline`, then add the
SQL plugin with `autoApply` so migrations run on startup:

```typescript
import { application, type Plugin } from "@putnami/application";
import { sql, sqlSourceInline } from "@putnami/database";
import type { MigrationContributor } from "@putnami/migration";

const migrations: Plugin & MigrationContributor = {
  migrationSources: () => [
    sqlSourceInline({
      namespace: "app",
      datasource: { name: "default", schema: "public" },
      definitions: [CreateUsersTable],
    }),
  ],
};

const app = application()
  .use(sql({ autoApply: true }))
  .use(migrations);

await app.start();
```

Migrations run automatically when your application starts. In production,
leave `autoApply` off (the default) and drive migrations with the `migrate`
CLI instead.

## Step 7: Raw SQL Queries (Optional)

For complex queries, you can use raw SQL:

```typescript
import { database } from "@putnami/database";

const sql = await database();

// Execute a raw query
const results = await sql`
  SELECT u.*, COUNT(o.id) as order_count
  FROM users u
  LEFT JOIN orders o ON o.user_id = u.id
  WHERE u.status = ${"active"}
  GROUP BY u.id
  ORDER BY order_count DESC
  LIMIT ${10}
`;
```

## Next Steps

- Learn about [Table Definitions](./entities.md) for advanced configuration
- Explore the [Repository Pattern](./repository.md) for all available methods
- Understand [Migrations](./migrations.md) for managing schema changes
- Configure [Database Connections](./connections.md) for multiple datasources
- Monitor queries with [Observability](./observability.md) for telemetry metrics and structured logging
- Use [Session Store](./session-store.md) for database-backed sessions

## Common Patterns

### Auto-generating Primary Keys

```typescript
const UsersTable = Table("users", {
  id: Key(Uuid, { autoGenerate: true }), // Will be auto-generated if not provided
  // ...
});
```

### Custom Column Names

```typescript
const UsersTable = Table("users", {
  id:       Key(Uuid),
  fullName: Column(String, { columnName: "full_name" }), // Maps to "full_name" column
});
```

### Using Named Datasources

```typescript
const UsersTable = Table("users", {
  id:    Key(Uuid),
  email: Column(Email),
}, {
  db: "auth", // Uses database.auth config
});
```

## Troubleshooting

### Connection Issues

- Verify your database is running: `pg_isready -h localhost -p 5432`
- Check credentials in your config file
- Ensure the database exists: `createdb myapp`

### Migration Issues

- Check migration names are unique and follow timestamp format
- Verify SQL syntax in migrations
- Check `migration.migrations` table for execution history

### Type Issues

- Ensure you are using `InferTable<typeof MyTable>` to derive entity types
- Verify that schema primitives (`String`, `Uuid`, `Email`, etc.) are imported from `@putnami/database`
- No `reflect-metadata`, `experimentalDecorators`, or `emitDecoratorMetadata` configuration is needed
