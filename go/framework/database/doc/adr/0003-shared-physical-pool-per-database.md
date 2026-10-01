# ADR 0003 — Share one physical pool per database and set search_path at acquire

- **Status**: accepted
- **Scope**: `go.putnami.dev/database` (`go/framework/database`); the shared
  rules also bind `@putnami/database`
  ([TypeScript ADR 0003](../../../../../typescript/framework/database/doc/adr/0003-shared-physical-pool-per-database.md))

## Context

A datasource is one schema of one database. One pool per datasource makes the
connection budget scale with schemas, not databases: a workload reading 21
schemas of one database holds 21 pools, each with idle backends and its own
ceiling. The pools differ only in `search_path`.

Unqualified queries rely on `search_path`. If a reused connection keeps
another datasource's value, such a query silently hits the wrong schema.
Consumers also use the raw pool surface (`Begin`, `Acquire`, `Exec`) and run
unqualified queries in those transactions.

## Decision

### Shared rules (Go and TypeScript)

1. **Physical identity is the connection, not the datasource.** Two
   datasources whose effective connection identity is identical share one
   physical pool, whatever the open order. Concurrent first opens of one
   identity connect once. The identity carries the password and never appears
   in a log or an error; diagnostics name datasources only.
2. **Tuning must agree.** A datasource that resolves to a physical pool
   another datasource created with different tuning fails with a declaration
   error naming both datasources and every differing field. Never the max,
   never a second pool.
3. **`search_path` is a session setting the framework owns.** The physical
   pool has no `search_path` startup parameter (`statement_timeout` stays
   one). Every operation runs on a connection whose `search_path` was set, or
   reset for a datasource on the server default, for the acquiring datasource.
   An operation whose set fails never returns a result.
4. **Reference-counted lifetime.** Logical pools hold references on their
   physical pool. Closing one datasource releases one handle; the physical pool
   closes with the last handle. Closing all releases everything, and a later
   open re-opens.
5. **Transactions are unchanged.** They stay keyed by the logical datasource
   ([ADR 0001](0001-pool-scoped-transaction-boundaries.md)). Two datasources of
   one database in one request hold two connections of one physical pool.
6. **User code must not issue a session-level `SET search_path`,
   `RESET search_path` or `DISCARD ALL`** on a framework connection.
   Transaction-local overrides (`SET LOCAL`, `set_config(…, true)`) are fine.

### Go implementation

- `Pools` keys the physical `*pgxpool.Pool` by the rendered DSN of each
  datasource's effective `PoolConfig`, after binding resolution and defaults.
- Compared tuning: `MaxConns`, `MinConns`, `MaxConnLifetime`,
  `MaxConnIdleTime`, `HealthCheckPeriod`, `StatementTimeout`,
  `IdentityResolver` and `TokenFetcher` (by code identity, nil-aware). The
  error is `db.datasource`, raised when the second datasource's pool is first
  opened. `ConnectTimeout`, `SlowQueryThreshold`, observers, `SearchPath` and
  infra fields are per logical pool.
- Every physical pool, registry or standalone `NewPool`, sets
  `pgxpool.Config.PrepareConn`. The acquire context carries the datasource's
  request as "present, possibly empty"; the hook compares it with a marker in
  the connection's `CustomData`:
  - equal: no statement;
  - different and non-empty: `SELECT set_config('search_path', $1, false)`,
    then update the marker;
  - empty over a non-empty marker: `RESET search_path`, clear the marker;
  - set or reset fails: `(false, error)`, the connection is destroyed and the
    query fails;
  - no request: `(true, error)` with `db.connection: connection acquired
    without a datasource`; the connection returns untouched.
  pgxpool hands out idle connections only and destroys one released
  mid-transaction, so the marker stays truthful across the migration runner's
  transaction-local `set_config(…, true)`.
- `Pool.PGXPool()` returns a datasource-bound `*database.PGXPool` (`Acquire`,
  `AcquireFunc`, `Begin`, `BeginTx`, `Exec`, `Query`, `QueryRow`, `SendBatch`,
  `CopyFrom`, `Ping`, `Stat`, `Config`) that satisfies `Querier`, never the raw
  `*pgxpool.Pool`. It is nil when the pool is not open. `Pool.Acquire(ctx)` is
  the explicit advanced surface.
- The migration runner and `ApplyToPool` open `database/sql` through a
  `driver.Connector` that marks each `Connect` context, with
  `MaxIdleConns(0)`. `NewPool`'s post-create ping is marked too.
- `Pool.Close()` releases its handle once. `Pools.Close()` is idempotent, drops
  both caches, and a later `For` re-opens. `Pools.mu` is never held across a
  connect.

## Rejected alternatives

- **One pool per datasource.** The budget scales with schemas.
- **Take the largest tuning on disagreement.** Silent: nobody knows which
  declaration won.
- **`search_path` as a startup parameter of the shared pool.** The first
  datasource's value would bind every other datasource's queries.
- **Schema-qualify every query.** User-written SQL cannot be made safe that
  way.
- **Return the raw `*pgxpool.Pool` from `PGXPool()`.** Every acquire on it
  would skip the set.
- **`BeforeAcquire`.** Deprecated in pgx v5.9.1 for `PrepareConn`, which can
  also fail the query with a typed error.
- **Query the server's `search_path` on each acquire.** One round trip per
  acquire; the marker makes the common case free.

## Consequences

- Pool statistics and `MaxConns` describe the shared physical pool, sized for
  the database.
- A workload that wants a smaller pool for one datasource declares it on every
  datasource of that database, or gives it a different connection identity.
- Hooks compare by code identity: two closures of one function literal with
  different captures compare equal. Declare hooks as shared functions, and give
  a datasource with a different principal a different connection identity.
- PostgreSQL re-analyzes a prepared statement when `search_path` changes. A
  same-named relation whose result shape differs between two schemas fails
  once with `cached plan must not change result type`. This is a known limit.
