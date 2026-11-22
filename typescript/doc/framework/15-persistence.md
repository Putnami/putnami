# Persistence

Use `@putnami/database` for PostgreSQL integration with fluent table definitions, repositories, and migrations. Build type-safe data access layers with a familiar repository pattern.

If your data is document-shaped rather than relational, use [`@putnami/document`](/docs/frameworks/typescript/document) instead.

## Getting started

### Installation

```bash
bunx putnami deps add @putnami/database
```

### Enable the SQL plugin

```ts
import { application, http } from '@putnami/application';
import { sql } from '@putnami/database';

export const app = () =>
  application()
    .use(http())
    .use(sql({ autoApply: true }))
    .use(usersMigrations); // see Migrations below
```

`autoApply: true` applies pending migrations at startup, before the server
listens. Without it, `sql()` applies none.

### Database configuration

The connection of the datasource `default` comes from the first of these that
is present:

1. A database binding: the `database.databases` section of the configuration,
   or the `DATABASE_BINDINGS` environment variable. A deploy target injects one
   of them.
2. The `database.default` section of the configuration.

For local development, put the connection in `conf/.env.local.yaml`:

```yaml
database:
  default:
    host: localhost
    port: 5432
    database: myapp
    user: postgres
    password: your_password
```

The framework reads no `DATABASE_URL` or `DATABASE_HOST` variable. To pass a
plain Postgres URL at run time, set `DATABASE_BINDINGS` to a binding document:

```bash
DATABASE_BINDINGS='{"protocolVersion":1,"databases":{"default":{"engine":"postgres","schema":"public","connection":{"dsn":"postgres://user:pass@127.0.0.1:5432/myapp"}}}}'
```

Or reference the variable from a config file: string values expand `${VAR}`
from the environment.

```yaml
database:
  protocolVersion: 1
  databases:
    default:
      engine: postgres
      schema: public
      connection:
        dsn: ${DATABASE_URL}
```

A compiled binary or container image ships no `conf/` directory. There, use
`DATABASE_BINDINGS` or `CONFIG_DATA`; see
[Publish & Docker](/docs/frameworks/typescript/extension/publish-and-docker).

### Configuration options

```yaml
database:
  default:
    host: localhost           # Database host; a path starting with / is a Unix socket directory
    port: 5432                # Database port
    database: myapp           # Database name
    user: postgres            # Username
    password: secret          # Password
    ssl: false                # true or false; on by default for remote TCP hosts
    poolSize: 10              # Maximum connections
    queryProfiling: false     # Log all queries
    debug: false              # Debug mode
    slowQueryThresholdMs: 0   # Warn on slow queries (ms, 0 = disabled)
    statementTimeoutMs: 30000 # statement_timeout (ms, 0 = disabled)
```

## Defining tables

A table definition maps a TypeScript shape to an existing table. It creates no
table: a migration owns the table, its SQL types and its defaults.

### Basic table

```ts
import { Table, Column, Key, Uuid, Email } from '@putnami/database';
import type { InferTable } from '@putnami/database';

const UsersTable = Table('users', {
  id:    Key(Uuid),
  email: Column(Email),
  name:  Column(String),
});

export type User = InferTable<typeof UsersTable>;
```

### Table with column options

```ts
import { Table, Column, Key, Uuid, Optional, DateIso } from '@putnami/database';
import type { InferTable } from '@putnami/database';

const ProductsTable = Table('products', {
  id:          Key(Uuid),
  name:        Column(String, { columnName: 'product_name' }),
  price:       Column(Number),
  description: Column(Optional(String)),
  active:      Column(Boolean, { default: true }),
  createdAt:   Column(DateIso, { columnName: 'created_at' }),
});

export type Product = InferTable<typeof ProductsTable>;
```

### Column options

- **columnName** - Database column name. The default is the property name in snake_case: the connection turns `createdAt` into `created_at`. Set it when the column has another name.
- **default** - Makes the field optional when `save(..., { strict: true })` validates it. It is never sent to SQL: declare the default in the migration.
- **toDatabase** - Transform value before saving
- **fromDatabase** - Transform value after loading

### Key options

- **columnName** - Database column name for the primary key
- **autoGenerate** - Makes the key optional under strict validation. `save()` still needs a key value: generate the id in code (`crypto.randomUUID()`), or insert with SQL and `RETURNING` (see [Direct database access](#direct-database-access)).

### Optional columns

Use `Optional()` to mark a column as nullable:

```ts
import { Optional } from '@putnami/database';

const UsersTable = Table('users', {
  id:  Key(Uuid),
  bio: Column(Optional(String)), // Nullable -- inferred as string | undefined
});
```

### Raw rows are camelCase

Queries through `this.conn()` return rows with camelCase keys: the column
`created_at` comes back as `createdAt`. Filters and `orderBy` take property
names (`'createdAt DESC'`), not column names.

## Migrations

### Inline migrations

A migration is a name, its SQL, and an optional `down`. Group migrations in a
source with `sqlSourceInline`, and contribute the source from a plugin's
`migrationSources`:

```ts
import { sqlSourceInline } from '@putnami/database';

const CreateUsersTable = {
  name: '20250101000000-create-users-table',
  sql: `
    CREATE TABLE IF NOT EXISTS users (
      id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
      email VARCHAR(255) UNIQUE NOT NULL,
      name VARCHAR(255) NOT NULL,
      created_at TIMESTAMPTZ NOT NULL DEFAULT now()
    );
  `,
  down: `DROP TABLE IF EXISTS users;`,
};

const AddUserAvatar = {
  name: '20250102000000-add-user-avatar',
  sql: `ALTER TABLE users ADD COLUMN IF NOT EXISTS avatar_url TEXT;`,
  down: `ALTER TABLE users DROP COLUMN IF EXISTS avatar_url;`,
};

export const usersMigrationSource = sqlSourceInline({
  namespace: 'users',
  datasource: { name: 'default', schema: 'public' },
  definitions: [CreateUsersTable, AddUserAvatar],
});

export const usersMigrations = {
  name: 'users-migrations',
  migrationSources: () => [usersMigrationSource],
};
```

Register the plugin next to `sql({ autoApply: true })`, as in
[Enable the SQL plugin](#enable-the-sql-plugin). TypeScript has no loader for
`.up.sql` files: declare each migration inline.

### Migration naming convention

Use timestamp prefixes for ordering. Migrations run in name order, per
datasource; the namespace becomes a prefix (`users/20250101000000-create-users-table`).

```text
YYYYMMDDHHMMSS-description
20250101000000-create-users-table
20250101120000-add-user-roles
20250102000000-create-posts-table
```

### Running migrations

With `sql({ autoApply: true })`, the application applies pending migrations
when it starts, before the server listens, and records each one in the
`migration.migrations` table. A restart skips the applied ones. The Go runner
shares this table and its lock, so both languages can target one database.

### Rollback

Add a `down` property to make migrations reversible. Run the migration command
against the same application builder the server uses:

```ts
// src/migrate.ts
import { runMigrateAndExit } from '@putnami/migration/cli';
import { app } from './main';

await runMigrateAndExit(app);
```

```bash
bun src/migrate.ts down     # roll back the latest migration
bun src/migrate.ts status   # list known and applied migrations
```

### Test against Postgres

`putnami test` starts a throwaway Postgres and passes it to the tests in
`DATABASE_TEST_BINDINGS`. `provision()` creates an empty, isolated database
for the suite. Start the application on it with `createTestApp`, and let
`sql({ autoApply: true })` apply the migrations:

```ts
import { afterAll, beforeAll, expect, it } from 'bun:test';
import { createTestApp, type TestApp } from '@putnami/application/testing';
import { api } from '@putnami/application';
import { provision, sql } from '@putnami/database';
import { usersMigrations } from '../src/db/users';

let db: Awaited<ReturnType<typeof provision>>;
let testApp: TestApp;

beforeAll(async () => {
  db = await provision();
  testApp = await createTestApp({
    plugins: [sql({ autoApply: true }), usersMigrations, api({ scanPath: 'src/api' })],
  });
});

afterAll(async () => {
  await testApp?.stop();
  await db?.cleanup();
});

it('lists users', async () => {
  const res = await testApp.fetch('/users');
  expect(res.status).toBe(200);
});
```

`provision({ sources })` applies those sources only when the test binding sets
`applyMigrations`; the binding `putnami test` passes does not.

`putnami test` starts the database only for a project that commits an
`infra/requirements.json` declaring one. The build writes that file when the
project uses `sql()`. See [Testing](/docs/frameworks/typescript/testing).

## Repository pattern

### Creating a repository

```ts
import { Repository } from '@putnami/database';

export class UserRepository extends Repository<typeof UsersTable> {
  constructor() {
    super(UsersTable);
  }
}
```

### Basic CRUD operations

```ts
const repo = new UserRepository();

// Create (strict: true validates all required fields are present)
const user = await repo.save({
  id: crypto.randomUUID(),
  email: 'alice@example.com',
  name: 'Alice',
}, { strict: true });

// Read by primary key
const found = await repo.get({ id: user.id });

// Update (partial — only validates provided fields)
const updated = await repo.save({
  ...found,
  name: 'Alice Smith',
});

// Delete
await repo.delete({ id: user.id });
```

### Finding records

```ts
const repo = new UserRepository();

// Find one by filter
const user = await repo.findOne({ email: 'alice@example.com' });

// Find multiple
const users = await repo.find({ active: true });

// Find with options
const pagedUsers = await repo.find(
  { active: true },
  {
    limit: 10,
    offset: 0,
    orderBy: 'createdAt DESC',
  }
);

// Check existence
const exists = await repo.exists({ email: 'alice@example.com' });
```

## Query operators

### Comparison operators

```ts
const repo = new UserRepository();

// Greater than
const recent = await repo.find({
  createdAt: { gt: new Date('2025-01-01') },
});

// Less than or equal
const old = await repo.find({
  age: { lte: 30 },
});

// Not equal
const notAdmin = await repo.find({
  role: { not: 'admin' },
});

// Range (multiple operators on same field)
const midRange = await repo.find({
  price: { gte: 100, lte: 500 },
});
```

### Available operators

| Operator | Description |
|---|---|
| `equals` | Equal (explicit form of direct value) |
| `not` | Not equal |
| `gt` | Greater than |
| `lt` | Less than |
| `gte` | Greater than or equal |
| `lte` | Less than or equal |
| `in` | Value in array |
| `notIn` | Value not in array |
| `like` | Pattern match (case-sensitive) |
| `ilike` | Pattern match (case-insensitive) |
| `isNull` | Check for NULL (`true` to match) |
| `isNotNull` | Check for NOT NULL (`true` to match) |

### Array operators

```ts
// in operator
const admins = await repo.find({
  role: { in: ['admin', 'superadmin'] },
});

// notIn operator
const regularUsers = await repo.find({
  role: { notIn: ['admin', 'superadmin'] },
});
```

### Pattern matching

```ts
// like (case-sensitive)
const matchingUsers = await repo.find({
  name: { like: 'John%' },
});

// ilike (case-insensitive)
const caseInsensitive = await repo.find({
  email: { ilike: '%@example.com' },
});
```

### NULL checks

```ts
// Find records with NULL value
const noAvatar = await repo.find({
  avatarUrl: { isNull: true },
});

// Find records with non-NULL value
const hasAvatar = await repo.find({
  avatarUrl: { isNotNull: true },
});
```

### Logical operators

```ts
// OR condition
const activeOrAdmin = await repo.find({
  $or: [
    { active: true },
    { role: 'admin' },
  ],
});

// AND condition (implicit when using multiple fields)
const activeAdmin = await repo.find({
  active: true,
  role: 'admin',
});

// Explicit AND
const complexQuery = await repo.find({
  $and: [
    { active: true },
    { createdAt: { gt: new Date('2025-01-01') } },
  ],
});

// Combined OR and AND
const combined = await repo.find({
  active: true,
  $or: [
    { role: 'admin' },
    { role: 'moderator' },
  ],
});
```

## Bulk operations

### Save many

```ts
const users = [
  { id: crypto.randomUUID(), email: 'user1@example.com', name: 'User 1' },
  { id: crypto.randomUUID(), email: 'user2@example.com', name: 'User 2' },
  { id: crypto.randomUUID(), email: 'user3@example.com', name: 'User 3' },
];

// Saves in batches of 100
await repo.saveMany(users);
```

### Delete many

```ts
// Delete all inactive users
await repo.deleteMany({ active: false });

// Delete with complex filter
await repo.deleteMany({
  lastLoginAt: { lt: new Date('2024-01-01') },
});
```

## Custom queries

### Extending the repository

```ts
import { Repository, database } from '@putnami/database';

export class UserRepository extends Repository<typeof UsersTable> {
  constructor() {
    super(UsersTable);
  }

  async findByEmailDomain(domain: string): Promise<User[]> {
    const db = await database();
    const result = await db`
      SELECT * FROM users
      WHERE email LIKE ${'%@' + domain}
      ORDER BY created_at DESC
    `;
    return result as User[];
  }

  async countByRole(): Promise<{ role: string; count: number }[]> {
    const db = await database();
    return db`
      SELECT role, COUNT(*) as count
      FROM users
      GROUP BY role
    `;
  }

  async searchUsers(query: string, limit: number = 10): Promise<User[]> {
    const db = await database();
    return db`
      SELECT * FROM users
      WHERE name ILIKE ${'%' + query + '%'}
         OR email ILIKE ${'%' + query + '%'}
      LIMIT ${limit}
    `;
  }
}
```

### Direct database access

```ts
import { database } from '@putnami/database';

export async function runCustomQuery() {
  const db = await database();

  // Parameterized query (safe from SQL injection)
  const users = await db`
    SELECT u.*, COUNT(p.id) as post_count
    FROM users u
    LEFT JOIN posts p ON p.user_id = u.id
    WHERE u.active = ${true}
    GROUP BY u.id
    ORDER BY post_count DESC
    LIMIT ${10}
  `;

  return users;
}
```

For comprehensive patterns (JOINs, subqueries, CTEs, aggregations, window functions, abort safety, and performance tips), see the [Advanced Queries Guide](../../framework/database/doc/advanced-queries.md).

## Multi-database support

### Named connections

```yaml
# conf/.env.local.yaml
database:
  default:
    host: localhost
    database: main_db
  analytics:
    host: analytics-host
    database: analytics_db
```

```ts
import { Table, Column, Key, Uuid, database } from '@putnami/database';

// Default database
const mainDb = await database();

// Named database
const analyticsDb = await database('analytics');

// Use in table definition
const AnalyticsEventsTable = Table('events', {
  id:        Key(Uuid),
  eventType: Column(String),
  payload:   Column(String),
}, {
  db: 'analytics',
});
```

### Connection lifecycle

```ts
import { database, closeDatabase, closeAllDatabases } from '@putnami/database';

// Get or create connection
const db = await database();

// Close specific connection
await closeDatabase();
await closeDatabase('analytics');

// Close all connections (on shutdown)
await closeAllDatabases();
```

## Transactions

### runInTransaction (recommended)

Wrap service logic in `runInTransaction` for automatic commit/rollback. No transaction parameter is passed — repositories participate transparently.

```ts
import { runInTransaction } from '@putnami/database';

async function transferFunds(fromId: string, toId: string, amount: number) {
  await runInTransaction(async () => {
    const from = await accountRepo.findOne({ id: fromId });
    const to = await accountRepo.findOne({ id: toId });

    await accountRepo.save({ ...from, balance: from.balance - amount });
    await accountRepo.save({ ...to, balance: to.balance + amount });
  });
  // Commits on success, rolls back on error
}
```

#### Transaction timeout

Set a timeout to auto-rollback transactions that take too long:

```ts
await runInTransaction(async () => {
  await orderRepo.save(order);
  await paymentService.charge(order); // slow external call
}, { timeoutMs: 5000 });
```

When the timeout fires, the transaction is rolled back and subsequent operations throw `"Transaction has timed out and was rolled back"`.

#### How transactions work

Transactions are lazy and write-aware:

- **Reads** use the normal connection pool (zero overhead).
- The **first write** reserves a connection and issues `BEGIN`.
- Subsequent reads and writes on the same database reuse the transaction connection.
- Multi-database operations get independent transactions per database.

### Manual primitives

For advanced control flow where the callback pattern doesn't fit:

```ts
import { withTransaction, commit, rollback } from '@putnami/database';

async function conditionalSave(items: Item[]) {
  withTransaction();

  for (const item of items) {
    await itemRepo.save(item);

    if (await isDuplicate(item)) {
      await rollback();
      return { status: 'duplicate' };
    }
  }

  await commit();
  return { status: 'ok' };
}
```

### Safety net middleware

Add `TransactionMiddleware` to auto-rollback any forgotten transactions at the end of a request and release connections on client disconnect:

```ts
import { TransactionMiddleware } from '@putnami/database';

http().use(TransactionMiddleware());
```

The middleware also listens for the request's `AbortSignal`. When a client disconnects or an HTTP timeout fires, in-flight queries are cancelled and reserved connections are released back to the pool.

## Observability

All repository operations emit structured logs and telemetry metrics automatically. When the `telemetry()` plugin is active, SQL metrics are collected alongside HTTP metrics with no additional setup.

### Metrics emitted

| Metric | Type | Description |
|---|---|---|
| `sql.{operation}.{table}` | Counter | Query count per operation and table |
| `sql.{operation}.{table}.duration` | Histogram | Query duration (ms) |
| `sql.query.duration` | Histogram | Aggregate across all tables |
| `sql.query.error` | Counter | Aggregate error count |
| `sql.query.slow` | Counter | Queries exceeding slow-query threshold |
| `sql.pool.created` | Counter | Connection pool creation events |
| `sql.pool.closed` | Counter | Connection pool close events |
| `sql.pool.count` | Gauge | Current active pool count |

### Structured logging

```text
[sql] find users { operation: "find", table: "users", duration: 12, rowCount: 42 }
[sql] save orders { operation: "save", table: "orders", duration: 8, rowCount: 1 }
```

### Slow query detection

```yaml
database:
  default:
    slowQueryThresholdMs: 200   # Warn on queries >= 200ms
```

```text
[sql] Slow query: find users (312ms >= 200ms) { operation: "find", table: "users", duration: 312, thresholdMs: 200 }
```

### Setup

```ts
import { application, http, telemetry } from '@putnami/application';
import { sql } from '@putnami/database';

const app = application()
  .use(telemetry({ bearer: process.env.TELEMETRY_TOKEN }))
  .use(sql())
  .use(http({ port: 3000 }));

await app.start();
// SQL metrics are now collected automatically
```

## Related guides

- [Add persistence](/docs/how-to/add-persistence)
- [Telemetry](/docs/frameworks/typescript/telemetry)
- [Add observability](/docs/how-to/add-observability)
- [Configuration](/docs/frameworks/typescript/configuration)

## Support and compatibility

`@putnami/database` and `@putnami/migration` are public, documented, maintained
packages classified `stable`. Their specifications and accepted decision records
live next to the package sources.

`@putnami/database` promises two things. The request is the transaction
boundary, and it opens lazily: transactional mode reserves no connection and
issues no `BEGIN` until the first write, the request-scoped `UnitOfWork` is the
sole committer, and a unit spanning several datasources is best-effort — a
partial commit is reported as an error, never as success. And migrations apply
in byte order of their name, identical to the Go runner that shares the same
state store, roll back in the exact reverse of that order rather than by
timestamp, and commit their body together with their bookkeeping row.

`@putnami/migration` promises to stay backend-free: it holds the cross-kind
contracts and the per-boot registry, exactly one runner registers per kind, and
migrations contributed for a kind with no runner fail by name instead of leaving
a store silently unmigrated.

Before v1.0.0, minor `0.x` releases may still contain documented breaking
changes; strict compatibility across every pre-1.0 minor is not promised.
