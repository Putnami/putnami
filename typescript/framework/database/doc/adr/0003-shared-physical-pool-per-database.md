# ADR 0003 — Share one physical pool per database and set search_path per operation

- **Status**: accepted
- **Scope**: `@putnami/database` (`typescript/framework/database`)

## Context

The shared rules (connection identity, tuning agreement, framework-owned
`search_path`, reference-counted lifetime, unchanged transactions, forbidden
session-level overrides) are owned by
[go/framework/database ADR 0003](../../../../../go/framework/database/doc/adr/0003-shared-physical-pool-per-database.md).
This record states how postgres.js implements them.

postgres.js has no acquire hook and does not expose which connection a query
or `reserve()` landed on, so the per-connection marker Go uses is impossible.
`reserve()` pins one connection, and statements dispatched on it without an
await in between are pipelined, so a `set_config` sent right before a statement
costs no extra round trip. The driver inlines a nested query as a fragment only
when it is an instance of its own `Query` class, which the Repository relies on
for upserts.

## Decision

1. **Identity.** `factory.ts` keys the physical `Sql` by the built options
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
   `Proxy` over the shared `Sql` (`postgres/datasource-client.ts`). A tagged
   template, `unsafe`, `file` and `notify` return the driver's own `Query`,
   re-pointed at a bound dispatch: when first awaited or executed, it
   `reserve()`s a connection, pipelines `SELECT set_config('search_path', $1,
   false)` (or `RESET search_path`) and the statement, and releases the
   connection when the statement settles. The set's verdict is tracked through
   the driver's settlement hooks; a statement whose set failed or did not
   answer first rejects with the set's error. Fragments, `values()`, `raw()`,
   `simple()`, `describe()`, `cursor()`, `forEach()`, `execute()` and
   `cancel()` keep native behavior; a query cancelled before dispatch reserves
   nothing. Builders forward untouched. `reserve()` sets the path before
   handing out the connection (on failure it releases and rethrows); `begin()`
   sets it before the callback. There is no marker and no skip.
4. **Lifetime.** `postgres/physical-pool.ts` counts handles.
   `closeDatabase(name)` or `client.end()` releases one handle per client; a
   closed client rejects operations naming the datasource. `sql.pool.created`,
   `sql.pool.closed` and `sql.pool.count` count physical pools.
   `collectPoolHealth` and `pingDatabase` go through the logical client.
5. **Transactions.** `useTxConnection` and `UnitOfWork` stay keyed by
   datasource ([ADR 0001](0001-the-request-is-the-transaction-boundary.md)).

## Rejected alternatives

- **A wrapper object standing in for the driver's `Query`.** The driver
  inlines only its own class, so Repository upsert fragments became `$n`
  parameters and failed with `syntax error at or near "$4"`.

## Consequences

- Every operation costs one `reserve()` plus one pipelined `set_config`; the
  driver's cross-connection query pipelining is replaced by per-operation
  reservation. Many statements on one connection use `reserve()` or `begin()`.
- The test configuration repeats identical tuning (`maxLifetime: 5`) on every
  datasource block of one database.
- The client relies on `Query` instance fields (`handler`, `executed`,
  `resolve`, `reject`, `cancelled`, `strings`). `test/shared-pool.test.ts`
  pins that shape on a real driver instance, so a driver upgrade that renames
  one fails there; `test/shared-pool.live.test.ts` covers behavior.
