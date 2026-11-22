# Database Connections

This guide covers how to configure and manage database connections, including multiple datasources, connection pooling, and advanced configuration options.

## Table of Contents

- [Basic Configuration](#basic-configuration)
- [Multiple Datasources](#multiple-datasources)
- [Connection Factory](#connection-factory)
- [Configuration Options](#configuration-options)
- [Connection Pooling](#connection-pooling)
- [Connection Cleanup](#connection-cleanup)
- [Raw SQL Queries](#raw-sql-queries)
- [GCP Integration](#gcp-integration)
- [Best Practices](#best-practices)

## Basic Configuration

### One Connection URL

Give the connection as one Postgres URL. Set `DATABASE_URL` in the
environment:

```bash
DATABASE_URL=postgres://postgres:your_password@localhost:5432/myapp
```

Or commit it under the datasource's `url` key, for example in
`conf/.env.local.yaml`:

```yaml
database:
  default:
    url: postgres://postgres:your_password@localhost:5432/myapp
    poolSize: 10
```

Each part the URL carries replaces the matching key of its section: host,
port, database, user, password, and `sslmode` for `ssl` (`disable` turns TLS
off, any other value turns it on). A part it omits, such as the port, keeps the
section's key or its default. Without `sslmode`, TLS is on for every host
except `localhost`, `127.0.0.1`, `::1` and Unix sockets, so add
`?sslmode=disable` for a server without TLS. Other query parameters, such as
`application_name`, become connection parameters, and `pool_max_conns` sets
`poolSize`. The tuning keys (`poolSize`, `maxRowLimit`, timeouts) still apply.
A URL that does not parse fails naming where it came from (`DATABASE_URL`,
`database.default.url`), never its value.

### Structured Fields

The section may instead spell the connection out:

```yaml
database:
  default:
    host: localhost
    port: 5432
    database: myapp
    user: postgres
    password: your_password
    ssl: false
    poolSize: 10
```

### Where the Connection Comes From

The connection of the primary datasource (`default`, unless `sql()` names
another) comes from the first of these that is present:

1. A database binding: the `database.databases` section of the configuration,
   or `DATABASE_BINDINGS`, a binding document such as
   `{"protocolVersion":1,"databases":{"default":{"engine":"postgres","schema":"public","connection":{"dsn":"postgres://user:pass@localhost:5432/myapp"}}}}`.
   A deploy target injects one; `provision()` injects one in tests.
2. The `url` key of the section, `database.default.url`.
3. The `DATABASE_URL` environment variable. It replaces the `host` and
   `database` keys a committed file sets, `conf/.env.test.yaml` included.
4. The structured fields of the section.

Every other datasource reads the same list without `DATABASE_URL`, since one
URL names one database: give `database('auth')` a `database.auth.url`. A
config object passed to `database(name, config)` with a `url` uses that URL
before all of these but the binding. No
`DATABASE_HOST`-style variable is read; a `${VAR}` reference in a config file
expands from the environment:

```yaml
database:
  auth:
    url: ${AUTH_DATABASE_URL}
```

### Programmatic Configuration

Configure connections programmatically:

```typescript
import { database, PostgresConfig } from "@putnami/database";
import type { InferConfig } from "@putnami/runtime";

// A programmatic configuration states every field, defaults included
const config: InferConfig<typeof PostgresConfig> = {
  host: "localhost",
  port: 5432,
  database: "myapp",
  user: "postgres",
  password: "your_password",
  ssl: false,
  poolSize: 10,
  debug: false,
  maxRowLimit: 10_000,
  slowQueryThresholdMs: 0,
  statementTimeoutMs: 30_000,
  idleInTransactionTimeoutMs: 300_000,
  connectTimeout: 10,
  idleTimeout: 60,
  maxLifetime: 0,
};

const sql = await database(undefined, config);
```

## Multiple Datasources

Configure multiple database connections for different purposes:

### YAML Configuration

```yaml
database:
  # Default connection
  default:
    host: localhost
    port: 5432
    database: myapp
    user: postgres
    password: password

  # Named datasources
  auth:
    host: auth-db.example.com
    port: 5432
    database: auth_db
    user: auth_user
    password: auth_password

  analytics:
    host: analytics-db.example.com
    port: 5432
    database: analytics_db
    user: analytics_user
    password: analytics_password
```

### Using Named Datasources

```typescript
import { database } from "@putnami/database";

// Default connection
const defaultDb = await database();

// Named datasources
const authDb = await database("auth");
const analyticsDb = await database("analytics");
```

### Tables with Named Datasources

Specify which datasource a table uses:

```typescript
import { Table, Column, Key, Uuid, Email } from "@putnami/database";

const UsersTable = Table("users", {
  id:    Key(Uuid),
  email: Column(Email),
}, {
  db: "auth", // Uses database.auth config
});

const EventsTable = Table("events", {
  id:        Key(Uuid),
  eventType: Column(String),
}, {
  db: "analytics", // Uses database.analytics config
});
```

## Connection Factory

The `database()` function is a connection factory that manages connection pooling and reuse:

```typescript
import { database } from "@putnami/database";

// Get or create a connection
const sql = await database();

// Clients are cached by datasource
// Multiple calls for the same datasource return the same client
const sql1 = await database();
const sql2 = await database();
// sql1 === sql2 (same client instance)
```

### One Pool per Database, Shared by Its Datasources

A datasource is one schema of one database. Datasources whose effective
connections are identical — same host, port, database, user, password source,
ssl and startup parameters — share **one physical connection pool** of
`poolSize` connections; only their `search_path` differs.

The pool opens connections as concurrent demand needs them: a connection is
opened only when every connection the pool holds is in use, and never more than
`poolSize`. The most recently released connection serves the next operation, so
the others go idle and close after `idleTimeout` seconds; an operation that
finds `poolSize` connections in use waits for one. A workload that runs one
query at a time holds one backend.

`database(name)` returns a *datasource-bound* client over that shared pool:

- every tagged-template query, `sql.unsafe(...)`, `sql.file(...)` and
  `sql.notify(...)` reserves a connection, sets the datasource's `search_path`
  on it (`SELECT set_config('search_path', $1, false)`, or `RESET search_path`
  for a datasource on the server default), runs the statement on that same
  connection and releases it. The set is pipelined with the statement — no
  extra network round trip — and a statement whose set failed rejects with the
  set's error;
- `sql.reserve()` and `sql.begin(...)` set the `search_path` on the pinned
  connection before handing it out, so everything you run on it resolves in
  the datasource's schema;
- `${sql(identifier)}` and `sql(rows, ...columns)` builders forward to the
  driver untouched.

`Bun.SQL` exposes no acquire hook and no connection identity, so — unlike
the Go adapter, which remembers the last value per connection and skips the
set when it matches — the TypeScript client sets the path on every operation.
It costs server-side microseconds.

Two datasources of one database **must declare the same pool tuning**
(`poolSize`, `connectTimeout`, `idleTimeout`, `maxLifetime`,
`statementTimeoutMs`, `idleInTransactionTimeoutMs`, `debug`). A disagreement
rejects `database(name)`:

```
database: datasources "auth" and "billing" resolve to the same physical database
but declare different pool tuning (poolSize 10 vs 4); a shared pool takes one
tuning — declare it identically on every datasource of that database
```

Because the pool is per database, a request that opens transactions on `k`
datasources of one database holds `k` connections of that ONE pool (it used
to hold one connection of each of `k` pools): size `poolSize` for the
database, not per datasource. See
[ADR 0003](./adr/0003-shared-physical-pool-per-database.md).

### Connection Registry

Connections are automatically cached and reused based on their configuration. The connection key is built from:
- Host/Socket
- Port
- Database name
- Username

This means:
- Same config = same connection (reused)
- Different config = new connection

## Configuration Options

### PostgresConfig

```typescript
interface PostgresConfig {
  url?: string;           // postgres:// URL; each part it carries replaces host, port, database, user, password or ssl
  host: string;           // Database host or Unix socket directory (default: 'localhost')
  port: number;           // Database port — ignored when host is a Unix socket (default: 5432)
  database: string;       // Database name (required unless url names it)
  user?: string;          // Database user — auto-resolved from GCP identity when omitted
  password?: string;      // Database password — auto-resolved as IAM token on socket hosts
  ssl: boolean;           // Enable SSL (default: false)
  poolSize: number;       // Connection pool size (default: 10)
  debug: boolean;         // Enable query debugging (default: false)
  queryProfiling?: boolean; // Enable query profiling (default: false)
  slowQueryThresholdMs: number; // Slow query warning threshold in ms (default: 0 = disabled)
}
```

### SSL Configuration

Enable SSL for secure connections:

```yaml
database:
  default:
    host: db.example.com
    port: 5432
    database: myapp
    user: postgres
    password: password
    ssl: true
```

### Unix Socket

Set `host` to the socket directory (the part before `.s.PGSQL.5432`):

```yaml
database:
  default:
    host: /var/run/postgresql
    database: myapp
    user: postgres
```

**Note:** When `host` starts with `/`, it is treated as a Unix socket
directory and `port` is ignored.

### Debug Mode

Enable query debugging:

```yaml
database:
  default:
    host: localhost
    database: myapp
    debug: true
```

When enabled, every SQL statement is logged before it runs, with each parameter
value redacted.

### Query Profiling

Enable query performance profiling:

```yaml
database:
  default:
    host: localhost
    database: myapp
    queryProfiling: true  # Enable query profiling
    # OR
    debug: true  # Also enables query profiling
```

When enabled, repository operations log:
- Query execution time
- Query descriptions
- Success/failure status

**Note:** Query profiling is automatically enabled for all repository methods when `queryProfiling` or `debug` is true.

### Slow Query Detection

Enable warnings for queries that exceed a duration threshold:

```yaml
database:
  default:
    host: localhost
    database: myapp
    slowQueryThresholdMs: 200  # Warn on queries >= 200ms
```

When a query exceeds the threshold, a `warn`-level log is emitted and the `sql.query.slow` telemetry counter is incremented. Set to `0` (default) to disable.

See [Observability](./observability.md) for the full metrics reference.

## Connection Pooling

### Pool Size

Configure the connection pool size:

```yaml
database:
  default:
    host: localhost
    database: myapp
    poolSize: 20  # Maximum number of connections in the pool
```

**Default:** 10 connections

**Recommendations:**
- Small applications: 5-10 connections
- Medium applications: 10-20 connections
- Large applications: 20-50 connections
- Very high traffic: 50-100 connections (monitor database limits)

### Connection Reuse

Connections are automatically reused from the pool:

```typescript
const sql = await database();

// All queries use connections from the pool
const users = await sql`SELECT * FROM users`;
const orders = await sql`SELECT * FROM orders`;
// Connections are returned to the pool after use
```

### Connection Lifecycle

1. **First Request** - The physical pool for that database is created (or shared, when another datasource of the same database already opened it)
2. **Subsequent Requests** - Connections are reused from the pool; each operation sets the datasource's `search_path` on the connection it uses
3. **Pool Exhaustion** - New requests wait for available connections
4. **Idle Connections** - Automatically closed after timeout
5. **Closing** - `closeDatabase(name)` releases that datasource's handle; the pool ends when its last datasource closes

## Connection Cleanup

The connection factory provides utilities to close database connections:

### `closeDatabase(name?: string): Promise<void>`

Close a specific database connection by name:

```typescript
import { closeDatabase } from "@putnami/database";

// Close named datasource
await closeDatabase("auth");

// Close default connection
await closeDatabase();
```

**Behavior:**
- Finds the connection by configuration
- Closes the connection and removes it from the registry
- Silently returns if connection doesn't exist
- Handles missing configurations gracefully

### `closeAllDatabases(): Promise<void>`

Close all database connections:

```typescript
import { closeAllDatabases } from "@putnami/database";

// Close all connections
await closeAllDatabases();
```

**Use Cases:**
- Application shutdown
- Testing cleanup
- Memory management
- Graceful shutdown handlers

**Example - Graceful Shutdown:**

```typescript
import { closeAllDatabases } from "@putnami/database";

process.on('SIGTERM', async () => {
  console.log('Shutting down...');
  await closeAllDatabases();
  process.exit(0);
});
```

## Raw SQL Queries

Use the connection directly for raw SQL queries:

```typescript
import { database } from "@putnami/database";

const sql = await database();

// Simple query
const users = await sql`
  SELECT * FROM users WHERE status = ${"active"}
`;

// Parameterized queries (safe from SQL injection)
const user = await sql`
  SELECT * FROM users WHERE id = ${userId} AND email = ${email}
`;

// Complex queries
const results = await sql`
  SELECT
    u.id,
    u.email,
    COUNT(o.id) as order_count,
    SUM(o.total) as total_spent
  FROM users u
  LEFT JOIN orders o ON o.user_id = u.id
  WHERE u.created_at > ${startDate}
  GROUP BY u.id
  HAVING COUNT(o.id) > ${minOrders}
  ORDER BY total_spent DESC
  LIMIT ${limit}
`;

// Transactions: every repository write and `this.conn()` query inside
// runInTransaction joins one transaction, committed on success
await runInTransaction(async () => {
  await userRepo.save({ id, email });
  await profileRepo.save({ userId: id, name });
});
```

### Query Tagging

Tag queries for better debugging:

```typescript
const sql = await database();

const users = await sql`
  SELECT * FROM users WHERE status = ${"active"}
`.then((rows) => {
  console.log(`Found ${rows.length} active users`);
  return rows;
});
```

## GCP Integration

The SQL module connects to Cloud SQL Postgres on GCP without a password
in config. The same configuration works on Cloud Run and on a dev laptop:

### Cloud Run

Deploy with `--add-cloudsql-instances=<instance>`. GCP mounts a Unix
socket at `/cloudsql/<instance>/.s.PGSQL.5432`. Point `host` at that
socket directory:

```yaml
database:
  default:
    host: /cloudsql/PROJECT:REGION:INSTANCE
    database: myapp
```

Because `host` is a Unix socket and no `password` is set, every connection
fetches an IAM access token from the metadata server and uses it as the
Postgres password.

### Dev laptop

Run [`cloud-sql-proxy`](https://cloud.google.com/sql/docs/postgres/connect-auth-proxy)
on a TCP port. The proxy authenticates outbound with your `gcloud auth
application-default login` credentials, so no password is needed:

```yaml
database:
  default:
    host: localhost
    port: 6543
    database: myapp
```

### User auto-resolution

When `user` is omitted, the framework resolves the active GCP IAM
principal at startup:

1. Metadata server (Cloud Run / GCE / Cloud Build) — service-account email
2. `gcloud config get-value account` — dev laptop fallback

The `.gserviceaccount.com` suffix is stripped (Postgres truncates role
names at 63 bytes; the Cloud SQL IAM role is created in the trimmed form).

**Requirements:**

- Service account with the `Cloud SQL Instance User` IAM role
- Cloud SQL instance with IAM authentication enabled

## Best Practices

### 1. Use Connection Pooling

Always use the connection factory to benefit from pooling:

```typescript
// ✅ Good - Uses connection pool
const sql = await database();
const users = await sql`SELECT * FROM users`;

// ❌ Bad - Creates new connection each time
// (Don't create postgres() directly)
```

### 2. Reuse Connections

Get the connection once and reuse it:

```typescript
// ✅ Good
const sql = await database();
const users = await sql`SELECT * FROM users`;
const orders = await sql`SELECT * FROM orders`;

// ❌ Less efficient (but still works)
const users = await (await database())`SELECT * FROM users`;
const orders = await (await database())`SELECT * FROM orders`;
```

### 3. Configure Pool Size Appropriately

Match pool size to your application's needs:

```yaml
# Development
database:
  default:
    poolSize: 5

# Production
database:
  default:
    poolSize: 20
```

### 4. Use Named Datasources for Separation

Separate concerns with named datasources:

```yaml
database:
  # Main application database
  host: localhost
  database: app_db

  # Read replica for analytics
  analytics:
    host: replica.example.com
    database: app_db
    # Use for read-only queries
```

### 5. Enable SSL in Production

Always use SSL for production databases:

```yaml
database:
  default:
    host: db.example.com
    ssl: true  # Required for production
```

### 6. Use Environment-Specific Configs

Different configurations for different environments:

```yaml
# .env.local.yaml (development)
database:
  default:
    host: localhost
    database: myapp_dev

# .env.production.yaml (production)
database:
  default:
    host: db.example.com
    database: myapp_prod
    ssl: true
```

### 7. Monitor Connection Usage

Connection pool lifecycle events are tracked automatically via telemetry metrics
(`sql.pool.created`, `sql.pool.closed`, `sql.pool.count`) and debug logs. When
the `telemetry()` plugin is active, pool creation and closure events are recorded
as counters, and the current pool count is maintained as a gauge without adding
default-level lifecycle noise.

See [Observability](./observability.md) for the full list of SQL metrics.

### 8. Handle Connection Errors

Implement proper error handling:

```typescript
try {
  const sql = await database();
  const users = await sql`SELECT * FROM users`;
} catch (error) {
  if (error instanceof Error) {
    if (error.message.includes("connection")) {
      // Handle connection errors
      console.error("Database connection failed:", error);
    }
  }
  throw error;
}
```

### 9. Use Transactions for Atomic Operations

Group related operations in transactions:

```typescript
import { runInTransaction } from '@putnami/database';

// Every repository write and `this.conn()` query inside joins one transaction
await runInTransaction(async () => {
  await users.save({ id, email });
  await profiles.save({ userId: id, name });
  // Both succeed or both fail
});
```

### 10. Close Connections on Shutdown

The connection factory manages connections automatically, but you can manually close them:

```typescript
import { database, closeDatabase, closeAllDatabases } from "@putnami/database";

// Close a specific database connection
await closeDatabase("auth");

// Close the default database connection
await closeDatabase();

// Close all database connections
await closeAllDatabases();
```

**When to use:**
- Application shutdown/cleanup
- Testing (close connections between tests)
- Dynamic datasource management
- Memory management in long-running processes

**Note:** In most cases, you don't need to manually close connections. The connection factory handles cleanup automatically. Connections are reused efficiently from the pool.
