# ADR 0006 — The pool remembers each connection's search_path

- **Status**: accepted
- **Scope**: `@putnami/database` (`typescript/framework/database`); amends
  decision 3 of [ADR 0003](0003-shared-physical-pool-per-database.md)

## Context

ADR 0003 sets `search_path` on every operation, because `Bun.SQL` names no
connection: a query or a `reserve()` could land on any of its pool's
connections. ADR 0005 then replaced `Bun.SQL`'s pool with the package's own:
each member is a single-connection `Bun.SQL` instance, so the package knows
which connection every statement runs on.

The per-operation set was not free. Every query reserved a connection,
created a proxy over it and sent a `set_config` before its statement. Profiled
under load, a point `SELECT` through a datasource-bound client spent about a
third of its CPU in that path. Most workloads hold one datasource per
database, so the set almost never changes anything.

`Bun.SQL` 1.4.5 reports every close of a connection through `onclose`: the
idle timeout, the lifetime, a server error or a network error. It fails the
statements already sent on a closed connection; it does not replay them on
the next one. A caller still waiting for a connection gets a new one. The
client sends nothing on a connection before its path is verified, except the
set itself, whose outcome it checks against the close count.

## Decision

1. **The pool records the path.** For each member, the driver pool keeps the
   `search_path` its connection's session holds, or "unknown". A close,
   reported by `onclose`, makes it unknown and counts the close. So do a
   `reserve()` and a transaction once released, since the caller's statements
   (or a rollback) may have changed the session.
2. **An operation sets the path only when it differs.** A datasource-bound
   operation leases a member, with no `reserve()`. When the member's session
   holds the datasource's path, the statement goes alone. Otherwise the client
   awaits the set (or `RESET search_path`), records the path, then sends the
   statement. A close counted while the set ran makes the client set it again,
   at most twice.
3. **Fail closed.** A failed set rejects the operation before its statement
   is sent. ADR 0003 pipelined the set before the statement; a failed set then
   still let the statement run under the previous path.
4. **Lifetime.** A member's connection closes after `maxLifetime` seconds.
   When the configuration leaves it at 0, each member draws a lifetime between
   45 and 90 minutes, as postgres.js did: `Bun.SQL`'s own default never closes
   a connection, and each connection keeps one prepared statement per distinct
   statement text.
5. **Bun floor.** The package needs Bun 1.4.5 (`engines.bun`); opening a pool
   on an older Bun fails with an error naming both versions.

## Rejected alternatives

- **Keep the per-operation set, pipelined.** It costs a reservation and a
  statement on every query to cover a case most workloads never have.
- **Check the session's path with a query.** It costs the round trip the
  cache saves.

## Consequences

- A workload with one datasource per database sends one set per connection,
  not one per query.
- Datasources that alternate on one connection pay one awaited round trip at
  each switch, where the pipelined set cost none.
- Statement code that changes `search_path` on a pooled connection outside
  `reserve()` and `begin()` is still not guarded (ADR 0003, non-goal), and the
  damage now lasts longer. Before, the next operation set the path again.
  Now the change holds on that connection until it closes, up to 90 minutes,
  or until another datasource, `reserve()` or `begin()` takes it. Change the
  path inside `reserve()` or `begin()`, or with `set_config(..., true)`.
