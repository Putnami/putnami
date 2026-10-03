# @putnami/database

PostgreSQL integration with declarative tables, repository pattern, and migrations.

## Setup

```ts
import { application } from '@putnami/application';
import { sql } from '@putnami/database';

export const app = () =>
  application()
    .use(sql());  // Registers database plugin, runs migrations on startup
```

Configuration in `conf/.env.local.yaml` (the `default` datasource):
```yaml
database:
  default:
    host: localhost
    port: 5432
    database: mydb
    user: user
    password: pass
```

In a deployed/test environment the connection is instead injected as a canonical
binding — merged into the `database` config section, or via the legacy
`DATABASE_BINDINGS` env var (see
[Canonical datasource binding](#canonical-datasource-binding)) — which takes
precedence over this config.

## Table Definitions

Declarative, type-safe table schemas — no decorators, no classes:

```ts
import { Table, Column, Key, Uuid, Email, Int, Optional, Default, DateIso } from '@putnami/database';
import type { InferTable } from '@putnami/database';

const UsersTable = Table('users', {
  id:        Key(Uuid),
  email:     Column(Email).unique(),
  name:      Column(String),
  age:       Column(Optional(Int)),
  role:      Column(Default(String, 'user')),
  createdAt: Column(DateIso).name('created_at'),
});

type User = InferTable<typeof UsersTable>;
// { id: string; email: string; name: string; age?: number; role: string; createdAt: string }
```

### Column Builders

| Builder | Purpose |
|---------|---------|
| `Key(type)` | Primary key column |
| `Column(type)` | Regular column |
| `.unique()` | Add unique constraint |
| `.name('sql_name')` | Map to different SQL column name |
| `.nullable()` | Allow NULL |

### Schema Types

Same as `@putnami/application`: `String`, `Number`, `Boolean`, `Int`, `Uuid`, `Email`, `DateIso`, `Optional(T)`, `ArrayOf(T)`, `Default(T, value)`

### Named Datasources

```ts
const AuditTable = Table('audit_logs', { ... }, { db: 'audit' });
// Resolves the 'audit' datasource (binding first, then database.audit config)
```

### Canonical datasource binding

A named datasource resolves its physical connection from the canonical database
protocol (`go.putnami.dev/protocol/database`) — the **same** `Binding` contract
the Go adapter consumes, so connection semantics stay uniform across languages.
When the deploy target injects a managed binding (a `Binding` document keyed by
logical datasource name) — merged into the `database` section of the resolved
config, or via the legacy `DATABASE_BINDINGS` environment variable, with config
winning when both are present — the connection and owning schema for each
datasource come from it and are authoritative; the schema becomes the
datasource's `search_path`, set on every connection the datasource uses. With
no binding, behaviour is unchanged — the `database.<name>` config drives the
connection. Connection tuning (pool size, timeouts) always comes from
config/defaults.

Datasources are logical; pools are per database. Datasources whose effective
connections are identical (several owned schemas of one database) share ONE
physical connection pool of `poolSize` connections and must declare the same
tuning — a disagreement rejects `database(name)` with an error naming both
datasources and each differing field. `database(name)` returns a
datasource-bound client over that shared pool: every query, `unsafe`, `reserve()`
and `begin()` sets the datasource's `search_path` on the connection it uses
(pipelined with the statement, no extra round trip), so unqualified queries
resolve in the datasource's schema whichever datasource last used that
connection. Closing one datasource never ends another's connections; the pool
ends with its last datasource. See `doc/adr/0003-shared-physical-pool-per-database.md`.

```json
{
  "$schema": "https://putnami.dev/schemas/putnami-database-binding.json",
  "protocolVersion": 1,
  "databases": {
    "audit":   { "engine": "postgres", "schema": "audit",   "connection": { "host": "localhost", "port": 5432, "database": "audit", "user": "postgres", "password": "postgres", "ssl": false } },
    "billing": { "engine": "postgres", "schema": "billing", "connection": { "instance": "proj:region:billing-db", "database": "billing" } },
    "events":  { "engine": "postgres", "schema": "events",  "connection": { "dsn": "postgres://app:s3cret@10.0.0.5:5432/events?sslmode=require" } }
  }
}
```

Each entry declares exactly one transport strategy — `dsn`, structured `host`
(TCP), or Cloud SQL `instance` socket. A datasource missing from the binding, or
a malformed binding, fails loudly at connect time. Bindings carry secrets and
are never written into `infra/requirements.json`.

## Test database provider

`provision()` is the TypeScript cross-language test database provider — the
counterpart of Go's `go.putnami.dev/database/testprovider`. It turns a canonical
**test binding** (the same `DATABASE_TEST_BINDINGS` contract the Go provider
reads) into isolated, migrated databases per named datasource, against an
externally provided Postgres (`mode: require`).

```ts
import { provision, TestProviderSkip } from '@putnami/database';
import * as sqlModule from '../src/.gen/src/.sql.gen'; // generated SQLSources

let db: Awaited<ReturnType<typeof provision>>;
beforeAll(async () => {
  try {
    db = await provision({ sources: Object.values(sqlModule) });
  } catch (e) {
    if (e instanceof TestProviderSkip) return; // mode: skip
    throw e;
  }
});
afterAll(() => db?.cleanup());
// Tests then use repositories / database('auth') as usual — provision injected
// DATABASE_BINDINGS so they connect to the isolated, migrated databases.
```

`provision` creates an isolated database (or schema) per datasource, applies the
workload's migration sources with the existing machinery (a one-off
`MigrationRegistry` + `SQLRunner`, which routes each datasource to its own
database), sets the owning schema as the session `search_path`, and injects the
resolved runtime binding into `DATABASE_BINDINGS` — pinning it as the
programmatic override too, which outranks both deploy transports (a managed
`database` config-section binding included), so the suite always reaches the
isolated databases even when a managed binding is present in the test
environment. `reuse: 'bundle-template'` (with
database isolation) applies migrations once into a template database keyed by the
maintenance database, the datasource and the migration-source digest, and clones
per-suite databases from it (advisory-lock-guarded). A datasource the bundle has
no migrations for gets no template — it takes the fresh path — so two datasources
sharing one maintenance database can never clone each other's template.
`cleanup()` drops the per-suite databases/schemas and
restores `DATABASE_BINDINGS` and the override; the template is left warm.
A killed test process never runs `cleanup()`, so with database isolation and
without `keepDatabases`, `provision` first drops the per-suite databases of the
same base that have no open connection and are older than one hour. The name
`<base>_t_<creation time><random>` (8 + 8 lowercase hex digits) carries the age,
in the same format as the Go provider, so each reclaims what the other left
behind; a live suite's database is never dropped.
`mode: 'skip'` throws
`TestProviderSkip`; Docker `mode: 'auto'` is a later slice. Unit tests pay no
database cost — the live path runs only when `DATABASE_TEST_BINDINGS` is set.

`datasources` names the datasources a suite uses. When set, `provision` plans,
builds templates for, reclaims, provisions and tears down only those entries,
and the returned (and injected) binding carries only them, so a suite handed a
workspace-wide binding pays for the datasources it reads. A name the binding
lacks follows the binding's mode, as a missing binding does: `skip` throws
`TestProviderSkip`, `require` and `auto` fail and name the datasource. Omitted
or empty provisions every datasource. The Go provider's `Options.Datasources`
behaves the same; both run the shared corpus in
`protocols/database/conformance/test-provider-datasources.json`.

```ts
db = await provision({ sources: Object.values(sqlModule), datasources: ['auth'] });
```

## Repository

Type-safe CRUD with the Repository base class:

```ts
import { Repository } from '@putnami/database';

class UserRepository extends Repository<typeof UsersTable> {
  constructor() {
    super(UsersTable);
  }

  async findByEmail(email: string) {
    return this.findOne({ email });
  }
}
```

### Built-in Methods

| Method | Returns | Description |
|--------|---------|-------------|
| `get(keys)` | `T \| undefined` | Find by primary key |
| `findOne(filters)` | `T \| undefined` | Find first match |
| `find(filters, opts?)` | `T[]` | Find all matching (limit, offset, orderBy) |
| `exists(keys)` | `boolean` | Check existence by key |
| `save(entity)` | `T` | Insert or update (upsert) |
| `saveAll(entities)` | `T[]` | Batch upsert |
| `delete(keys)` | `void` | Delete by primary key |

### Find Options

```ts
const users = await userRepo.find(
  { role: 'admin' },
  { limit: 10, offset: 0, orderBy: 'created_at DESC' }
);
```

### Custom Queries

Resolve a transaction-aware connection with `this.conn(mode?)` for raw SQL within
repositories. It returns the `SqlClient` (putnami's driver-agnostic alias) for the repository's datasource and
automatically joins an active transaction / request `UnitOfWork`. Pass `'read'`
for read-only queries (zero-cost outside a write transaction):

```ts
class UserRepository extends Repository<typeof UsersTable> {
  constructor() { super(UsersTable); }

  async findActive() {
    const sql = await this.conn('read');
    return sql<User[]>`SELECT * FROM users WHERE active = true ORDER BY name`;
  }

  async countByRole(role: string) {
    const sql = await this.conn('read');
    const [row] = await sql<[{ count: number }]>`
      SELECT COUNT(*)::int AS count FROM users WHERE role = ${role}
    `;
    return row.count;
  }
}
```

## Migrations

SQL scripts tracked in `migration.migrations` table, executed on startup:

```ts
const CreateUsersTable = {
  name: '20250101000000-create-users-table',
  sql: `
    CREATE TABLE IF NOT EXISTS users (
      id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
      email VARCHAR(255) UNIQUE NOT NULL,
      name VARCHAR(255) NOT NULL,
      created_at TIMESTAMPTZ DEFAULT NOW()
    );
    CREATE INDEX IF NOT EXISTS idx_users_email ON users(email);
  `,
  down: `DROP TABLE IF EXISTS users;`,
};
```

### Registering Migrations

Contribute migrations from a plugin via `sqlSourceInline`, then enable the SQL plugin:

```ts
import { sql, sqlSourceInline } from '@putnami/database';
import type { Plugin } from '@putnami/application';
import type { MigrationContributor } from '@putnami/migration';

const migrations: Plugin & MigrationContributor = {
  migrationSources: () => [
    sqlSourceInline({ namespace: 'app', datasource: { name: 'default', schema: 'public' }, definitions: [CreateUsersTable] }),
  ],
};

application()
  .use(sql({ autoApply: true }))
  .use(migrations);
```

For the file-based `.up.sql` / `.down.sql` flow, see `doc/migrations.md`.

### Migration Naming

Use timestamp prefix: `YYYYMMDDHHMMSS-description`

Migrations run in alphabetical order. Each migration runs exactly once, keyed by
name. The body is hashed (sha256) at apply time and stored; on a subsequent
forward apply the committed body is re-hashed and compared, so editing an
already-applied migration's SQL aborts with a drift error instead of being
silently ignored. Create a new migration for schema changes rather than editing
an applied one.

### Migration bundles

`sqlBundleContributions(sources)` maps `SQLSource`s to `migration-bundle.v1`
operations + materialized payloads (pure, no DB), the counterpart of the Go SQL
runner. Combined with `@putnami/migration`'s `writeBundle`/`computeBundleDigest`,
this produces the same content-addressed bundle the Go runtime emits — the
digest is identical across languages.

## Transactions

`runInTransaction(fn)` is the recommended boundary. No `tx` parameter is passed —
every repository write (a `new Repository(...)`, an injected/`provideRepository`
one, or a `this.conn()` raw query) lazily joins one transaction per participating
datasource, and the callback commits on success / rolls back on error:

```ts
import { runInTransaction } from '@putnami/database';

await runInTransaction(async () => {
  await userRepo.save({ id, name: 'Alice' });
  await auditRepo.save({ action: 'user_created' });
  // Commit on success, rollback on error.
});
```

For low-level control use the `withTransaction()` + `commit()` / `rollback()`
primitives; for a per-request "transaction owns the request" boundary use the DI
request-scoped `UnitOfWork` + `UnitOfWorkMiddleware`. The optimistic-concurrency
helpers `consumeOnce` / `compareAndSet` / `rotateRow` return the typed cross-language
`Outcome` (`applied` / `already-consumed-conflict` / `not-found` /
`retryable-serialization-failure`). See `doc/transactions.md` for the full guide,
worked two-repository examples, and the non-atomic multi-datasource boundary.

## DI Integration

Register a repository as a DI provider with `provideRepository(tableDef)` and
resolve/inject it by `repositoryToken(tableDef)`. This is additive — the
`new Repository(tableDef)` path keeps working and still joins the ambient
transaction / request `UnitOfWork`.

```ts
import { provideRepository, repositoryToken } from '@putnami/database';

application()
  .use(sql())
  .register(provideRepository(UsersTable));

// In endpoints — resolve/inject by the table's repository token
endpoint()
  .inject({ users: repositoryToken(UsersTable) })
  .handle(async (ctx) => {
    return { users: await ctx.deps.users.find({}) };
  });

// In a service — declare the dependency so the composition names it
application().provide(UserService, { deps: [repositoryToken(UsersTable)] });
```

Declaring the repository is also what makes the data path visible to the design
graph a feature module derives: the token identity becomes the service an
operation or another provider injects, and the plugin relates it to its
`data.table` with `reads`/`writes`. A `new Repository(tableDef)` created inside a
service body keeps working and is still transactional, but no declaration names
it, so the graph shows the service without the table it touches.

## Detailed Documentation

See `doc/` folder:
- `entities.md` — table definitions, column types
- `repository.md` — repository API, custom queries
- `migrations.md` — migration system
- `transactions.md` — transaction support
- `connections.md` — connection config, pools
- `advanced-queries.md` — raw SQL, joins
- `observability.md` — query logging, metrics
- `session-store.md` — database-backed sessions

## Maintained contract

This public package is `stable` in the workspace [support
catalog](../../../putnami.support.json). It owns two specifications: the
[relational-persistence specification](specs/relational-persistence.json) with
its [request-boundary ADR](doc/adr/0001-the-request-is-the-transaction-boundary.md),
and the [SQL-migrations specification](specs/sql-migrations.json) with its
[order-is-the-name ADR](doc/adr/0002-migration-order-is-the-name-not-the-clock.md).

Facts to rely on when generating code:

- Transactional mode is lazy. `withTransaction()` flags the context; nothing is
  reserved and no `BEGIN` is issued until the first write.
- The request-scoped `UnitOfWork` is the sole committer. A nested
  `runInTransaction()` joins the outer unit; it never commits it. Use
  `setRollbackOnly()` to fail a unit while still returning a response.
- Multi-datasource units are best-effort, not two-phase commit. A partial commit
  is raised as an error (an `AggregateError` when several failed).
- Rollback telemetry carries a classified cause code only. Never add an error
  message, bound parameter, or row value to a transaction record.
- Migration order is byte order of the name, matching the Go runner that shares
  the state store. Never use `localeCompare` for migration names, and never
  order rollbacks by `executed_at`.

Before v1.0, follow the workspace
[migration-based compatibility policy](../../../RELEASE.md); do not infer strict
compatibility between every `0.x` minor.
