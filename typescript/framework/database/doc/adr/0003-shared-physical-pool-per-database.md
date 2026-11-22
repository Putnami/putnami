# ADR 0003 — Share one physical pool per database and set search_path per operation

- **Status**: accepted
- **Scope**: `@putnami/database` (`typescript/framework/database`)

## Context

The shared rules (connection identity, tuning agreement, framework-owned
`search_path`, reference-counted lifetime, unchanged transactions, forbidden
session-level overrides) are owned by
[go/framework/database ADR 0003](../../../../../go/framework/database/doc/adr/0003-shared-physical-pool-per-database.md).
This record states how the TypeScript adapter implements them on `Bun.SQL`
([ADR 0005](0005-bun-sql-is-the-driver.md)).

`Bun.SQL` has no acquire hook and does not expose which connection a query
or `reserve()` landed on, so the per-connection marker Go uses is impossible.
`reserve()` pins one connection, and statements executed on it back to back
are pipelined, so a `set_config` sent right before a statement costs no extra
round trip. The driver's identifier and row builders touch no connection, so
a builder made by the pool works inside a statement of any of its connections.

## Decision

1. **Identity.** `factory.ts` keys the physical pool by the built options
   minus tuning and `search_path`: host, port, database, resolved username,
   password source (the explicit password, `iam-token`, or `none`), ssl and the
   remaining startup parameters. An in-flight promise per identity makes
   concurrent first opens construct once.
2. **Tuning.** Compared fields: `poolSize`, `connectTimeout`, `idleTimeout`,
   `maxLifetime`, `statementTimeoutMs`, `idleInTransactionTimeoutMs`, `debug`.
   `database('B')` rejects on a difference, with Go's error text. Identity
   hooks are process-global in TypeScript, so there is nothing to compare.
3. **`search_path` per operation.** `idle_in_transaction_session_timeout`
   stays a startup parameter. `database(name)` returns a datasource-bound
   client over the shared pool (`postgres/datasource-client.ts`). A tagged
   template, `unsafe`, `file` and `notify` return a Putnami query that only
   captures its template: when first awaited or executed, it `reserve()`s a
   connection, executes `SELECT set_config('search_path', $1, false)` (or
   `RESET search_path`) and then the statement back to back on it, and
   releases the connection when the statement settles. A statement whose set
   failed rejects with the set's error and its result is never returned.
   Nested queries are rebuilt on the reserved connection; `values()`, `raw()`,
   `simple()` and `cancel()` reach the driver query; a query cancelled before
   dispatch reserves nothing. Builders are made by the pool and touch no
   connection.
   `reserve()` sets the path before handing out the connection (on failure it
   releases and rethrows); `begin()` sets it before the callback. There is no
   marker and no skip.
4. **Lifetime.** `postgres/physical-pool.ts` counts handles.
   `closeDatabase(name)` or `client.end()` releases one handle per client; a
   closed client rejects operations naming the datasource. `sql.pool.created`,
   `sql.pool.closed` and `sql.pool.count` count physical pools.
   `collectPoolHealth` and `pingDatabase` go through the logical client.
5. **Transactions.** `useTxConnection` and `UnitOfWork` stay keyed by
   datasource ([ADR 0001](0001-the-request-is-the-transaction-boundary.md)).

## Rejected alternatives

- **Re-pointing the driver's own query objects at a bound dispatch.** It is
  what the postgres.js implementation did, because that driver inlined only
  its own `Query` class as a fragment. It relied on private fields of the
  driver and is not possible on `Bun.SQL`, whose queries expose none; a
  captured template rebuilt on the dispatch connection needs none.
- **One pool per datasource with `search_path` as a startup parameter.** It
  multiplies idle backends by the number of datasources, which the shared
  rules exist to prevent.

## Consequences

- Every operation costs one `reserve()` plus one pipelined `set_config`; the
  driver's cross-connection query pipelining is replaced by per-operation
  reservation. Many statements on one connection use `reserve()` or `begin()`.
- The test configuration repeats identical tuning (`maxLifetime: 5`) on every
  datasource block of one database.
- The client relies on the driver accepting a builder made by the pool inside
  a statement of a reserved connection, and on in-order execution on a
  reserved connection. `test/shared-pool.live.test.ts` covers both against a
  real server.
