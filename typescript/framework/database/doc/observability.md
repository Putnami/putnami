# SQL Observability

Built-in observability for all database operations. Every repository method emits structured logs and telemetry metrics automatically — no manual instrumentation needed.

## Table of Contents

- [How it works](#how-it-works)
- [Telemetry metrics](#telemetry-metrics)
- [Structured logging](#structured-logging)
- [Slow query detection](#slow-query-detection)
- [Connection pool metrics](#connection-pool-metrics)
- [Configuration](#configuration)
- [Example: full observability stack](#example-full-observability-stack)

## How it works

Every repository operation (`find`, `save`, `delete`, `exists`, `saveMany`, `deleteMany`) is wrapped in an `observe()` hook that:

1. **Measures timing** — captures start/end for duration calculation
2. **Emits telemetry** — counters for query count/errors, histograms for duration/row counts
3. **Logs structured data** — the `database` group (operation, table, datasource, durationMs/durationUs, outcome, row count) at `debug` level
4. **Detects slow queries** — warns when queries exceed a configurable threshold

All telemetry calls are no-ops when the telemetry plugin is not registered, so there is zero overhead in environments without telemetry.

## Telemetry metrics

When the `telemetry()` plugin is active, the following metrics are emitted automatically:

### Query metrics

| Metric | Type | Description |
|---|---|---|
| `sql.{operation}.{table}` | Counter | Query count per operation and table |
| `sql.{operation}.{table}.duration` | Histogram | Query duration (ms) per operation and table |
| `sql.query.duration` | Histogram | Aggregate duration across all tables |
| `sql.{operation}.{table}.error` | Counter | Error count per operation and table |
| `sql.query.error` | Counter | Aggregate error count |
| `sql.query.slow` | Counter | Queries that exceeded the slow-query threshold |

**Operations:** `find`, `save`, `saveMany`, `delete`, `deleteMany`, `exists`

**Examples:**

```text
sql.find.users              — counter, incremented on each find() call
sql.find.users.duration     — histogram, tracks query latency
sql.save.orders             — counter
sql.save.orders.error       — counter, incremented on failures
sql.query.duration          — histogram, all queries combined
sql.query.slow              — counter, slow queries across all tables
```

### Connection pool metrics

| Metric | Type | Description |
|---|---|---|
| `sql.pool.created` | Counter | Pool creation events |
| `sql.pool.closed` | Counter | Pool close events |
| `sql.pool.count` | Gauge | Current number of active connection pools |

## Structured logging

All queries produce structured log entries under the pinned `database` logger.
Messages are **constants** and every domain field travels in one nested,
camelCase `database` group, so dashboards and log-based metrics can match on
them; the record shapes live in `src/observability/database-logging.ts` and the
contract is normative in `protocols/logging/conformance`. Every record carries
both `durationMs` and `durationUs` — sub-millisecond queries are the norm here.

### Successful queries (debug level)

```text
[database] query executed { database: { operation: "find", table: "users", datasource: "default", durationMs: 12, durationUs: 12000, outcome: "success", rowCount: 42 } }
```

### Failed queries (warn level)

```text
[database] query failed { error: { name: "PostgresError", message: "duplicate key..." }, database: { operation: "save", table: "users", datasource: "default", durationMs: 45, durationUs: 45210, outcome: "failure" } }
```

`warn`, not `error`: the data layer reports the outcome and latency and
**re-throws**, so the boundary that actually fails (the HTTP or event terminal
record) owns `error` severity exactly once — a retried-and-recovered query is not
error-level noise. The database error itself travels in the record's structured
`error` field, never as a stringified data field (which the JSON sink drops).

### Slow queries (warn level)

```text
[database] slow query { database: { operation: "find", table: "users", datasource: "default", durationMs: 312, durationUs: 312400, outcome: "success", thresholdMs: 200 } }
```

The query **succeeded** — slowness is a latency signal, not a failure — and the
breached threshold is a field rather than part of the message.

### Transaction boundaries (debug on commit, warn on rollback)

```text
[database] transaction committed    { database: { datasource: "default", outcome: "success", durationMs: 8, durationUs: 8120, retries: 0 } }
[database] transaction rolled back  { database: { datasource: "default", outcome: "failure", durationMs: 3, durationUs: 3400, retries: 0, rollbackCause: "23505" } }
```

One record per datasource that actually opened a transaction (a read-only
`runInTransaction` never opens one, so it has no boundary to report). The rollback
record carries the **classified, secret-free** `rollbackCause` only — a SQLSTATE,
a repository error code, an error class, or a sentinel such as `unknown` — and
never an error message, a bound parameter value, or row data.

### Migration records (`database.migration` logger)

```text
[database.migration] migration applied { migration: { name: "0001_init", datasource: "default", durationMs: 42, outcome: "success" } }
[database.migration] migration failed  { migration: { name: "0002_orders", datasource: "default", durationMs: 7, outcome: "failure" },
                                         error: { name: "PostgresError", message: "syntax error at end of input" } }
```

One terminal record per migration: `info` on success (previously these were
debug-silent) and `error` on failure, with the raw cause in the structured
`error` field while the migration still aborts the run. The runner's aggregate
`migrations applied { migration: { count, datasources } }` record is a separate,
coarser signal.

### Connection pool events (debug level)

```text
[database] Database pool created { datasource: "auth" }
[database] Database pool closed { datasource: "default" }
```

Pool creation and closure remain available as debug diagnostics, while their
counters and gauge are the default operator-facing signals. Slow queries remain
visible at `warn`.

### Conformance

These record shapes are pinned by executable golden tests against the shared
corpus in `protocols/logging/conformance`:
`test/logging-cross-language.test.ts` here and
`go/framework/database/logging_cross_language_test.go` in Go. The query and
transaction cases drive a real repository / transaction boundary against Postgres
and are gated on `DATABASE_TEST_BINDINGS` (skipped without it); the migration
cases drive the real migrator over a DB-free recording connection and always run.
Changing a field name, severity, or message in one runtime fails the build until
the corpus and the other runtime are updated in the same commit.

## Slow query detection

Enable slow-query warnings by setting a threshold in your database configuration:

```yaml
database:
  host: localhost
  database: myapp
  slowQueryThresholdMs: 200   # Warn on queries taking >= 200ms
```

When a query exceeds the threshold:
- A `warn`-level structured log is emitted with the query details and threshold
- The `sql.query.slow` telemetry counter is incremented

Set to `0` (the default) to disable slow-query detection.

## Connection pool metrics

The connection factory tracks pool lifecycle events:

- **`sql.pool.created`** increments when `database()` creates a new connection pool
- **`sql.pool.closed`** increments when `closeDatabase()` closes a pool
- **`sql.pool.count`** is a gauge reflecting the current number of active pools

These metrics help monitor connection pool churn and ensure pools are being reused properly.

## Configuration

### Full configuration reference

```yaml
database:
  host: localhost
  port: 5432
  database: myapp
  user: postgres
  password: secret
  ssl: false
  poolSize: 10
  debug: false                 # Log raw SQL queries with parameters
  queryProfiling: false        # Legacy profiling flag (superseded by observability)
  slowQueryThresholdMs: 200    # Warn on slow queries (0 = disabled)
```

| Option | Type | Default | Description |
|---|---|---|---|
| `debug` | `boolean` | `false` | Log raw SQL queries with parameters |
| `queryProfiling` | `boolean` | `false` | Legacy flag — observability is always active |
| `slowQueryThresholdMs` | `number` | `0` | Slow-query warning threshold in ms (0 = off) |

### Relationship to telemetry

Observability metrics are emitted automatically when the `telemetry()` plugin is registered on the application. No additional SQL-specific setup is needed:

```ts
import { application, http, telemetry } from '@putnami/application';
import { sql } from '@putnami/database';

const app = application()
  .use(telemetry({ bearer: process.env.TELEMETRY_TOKEN }))
  .use(sql())
  .use(http({ port: 3000 }));

await app.start();
// SQL metrics are now collected alongside HTTP metrics
```

## Example: full observability stack

```ts
import { application, http, telemetry } from '@putnami/application';
import { sql, Repository, Table, Column, Key, Uuid } from '@putnami/database';
import { useLogger } from '@putnami/runtime';

// 1. Define table
const OrdersTable = Table('orders', {
  id:     Key(Uuid),
  status: Column(String),
  total:  Column(Number),
}, {
  migrations: [{ name: '001-create-orders', sql: `CREATE TABLE IF NOT EXISTS orders (...)` }],
});

// 2. Create repository
class OrderRepository extends Repository<typeof OrdersTable> {
  constructor() { super(OrdersTable); }
}

// 3. Configure app with telemetry + SQL
const app = application()
  .use(telemetry({ bearer: process.env.TELEMETRY_TOKEN }))
  .use(sql())
  .use(http({ port: 3000 }));

await app.start();

// 4. Use the repository — all operations are automatically observed
const repo = new OrderRepository();

const order = await repo.save({ id: crypto.randomUUID(), status: 'pending', total: 99.99 });
// Telemetry: sql.save.orders +1, sql.save.orders.duration histogram
// Log: [database] query executed { database: { operation: "save", table: "orders", durationMs: 12, durationUs: 12000, outcome: "success", rowCount: 1 } }

const orders = await repo.find({ status: 'pending' });
// Telemetry: sql.find.orders +1, sql.find.orders.duration histogram
// Log: [database] query executed { database: { operation: "find", table: "orders", durationMs: 5, durationUs: 5100, outcome: "success", rowCount: 3 } }
```

## Programmatic access

The metric recording functions are exported from `@putnami/database` for use in custom repository methods or direct database access:

```ts
import { recordQuery, recordQueryError, type SqlOperation, type QueryMetrics } from '@putnami/database';

// Record a custom query. Measure with performance.now(): `duration` is in
// milliseconds and MAY be fractional, which is what lets the record carry a real
// `durationUs` alongside `durationMs`.
const start = performance.now();
try {
  const result = await db`SELECT ...`;
  recordQuery({
    operation: 'find',
    table: 'custom_view',
    duration: performance.now() - start,
    rowCount: result.length,
    datasource: 'default',
  });
} catch (error) {
  recordQueryError('find', 'custom_view', performance.now() - start, error, 'default');
  throw error;
}
```

## Related guides

- [Database Connections](./connections.md)
- [Repository Pattern](./repository.md)
- [Telemetry](/docs/frameworks/typescript/telemetry)
- [Observe a running app](/docs/how-to/observe-a-running-app)
