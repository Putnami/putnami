# ADR 0007 — Datasource operations share a connection

- **Status**: accepted
- **Scope**: `@putnami/database` (`typescript/framework/database`); amends
  [ADR 0006](0006-the-pool-remembers-each-connections-search-path.md)

## Context

ADR 0005 replaced `Bun.SQL`'s pool with the package's own: each member is a
single-connection `Bun.SQL` instance. ADR 0006 made an operation lease a member
and send its statement alone when the member's session holds the datasource's
`search_path`. A lease was exclusive: one operation per connection at a time.
Under concurrency, every query then paid a whole round trip of its own: one
statement written, one response read, one wake of the event loop. `Bun.SQL`'s
own pool lets several statements follow one another on a connection and reads
their responses together.

Measured on the tasks-api bench (a cloud container with 4 CPUs, Bun 1.4.5,
PostgreSQL 17 on the same host, wrk with 64 connections, CPU time of the
service per request, framework main b20ea09d):

| Route | Putnami | the same queries on `Bun.SQL`'s pool | on the driver pool alone |
|---|---|---|---|
| point `GET` | 53 µs | 34 µs | 57 to 61 µs |
| list | 90 µs | 62 µs | 94 to 96 µs |
| insert | 86 µs | 70 µs | 89 to 93 µs |

The driver pool alone accounts for the difference. The datasource client,
the repository and the transaction lookup cost close to nothing. Under
strace, a point `GET` read its connection 2.0 times per request on the driver
pool and 1.2 times on `Bun.SQL`'s pool, and woke the event loop 0.10 against
0.03 times.

## Decision

1. **A datasource operation shares a member whose session holds its path.** The
   client passes the datasource's `search_path` with its lease. An idle member
   whose session holds it is taken first, then any idle member, then a new one
   while the pool holds fewer than `poolSize`. When none is left, the
   operation joins the least busy member already shared under that path, and
   its statement follows the others on that connection.
2. **Anything that can change a session holds its member alone.** A member
   whose session holds another path, or none known, is leased alone, so the
   client sets the path before any statement goes on it. A `reserve()`, a
   `begin()` and a template call on the pool lease alone too, and wait until
   no shared operation runs on the member.
3. **Waiters keep their turn.** While an operation waits, a new one does not
   join a shared member: it waits behind it, so a stream of shared operations
   never starves a reservation, a transaction or another datasource's set. A
   member no operation uses goes to the first waiter. When that waiter shares
   it, the waiters right behind it that need the same path share it too, up to
   an even share of the waiting operations per member; the first waiter that
   needs it alone, or another path, stops the share.
4. **A stalled member takes no joiner.** An operation joins a shared member
   only when a statement finished on it, or it began to be shared, less than
   100 ms ago. A statement
   waiting on a lock then holds back only the statements that joined it
   already, and new operations wait for any member instead.
5. **The pool owns the lifetime.** Members get no `maxLifetime` from
   `Bun.SQL`. Measured on Bun 1.4.5: when `Bun.SQL` retires a busy connection
   at its lifetime, the statements sent after the retirement wait in its
   queue and run on a new session, at the server-default `search_path`, before
   `onclose` reports the close; five such statements read another schema's
   table. The pool draws each member's lifetime as ADR 0006 did; past it, the
   member takes no new operation, and it closes once none runs on it. A
   waiter then gets a new member.

ADR 0006's guarantees hold: no statement goes on a connection whose path is
not verified, and a failed set rejects its operation before its statement.

## Rejected alternatives

- **Return to `Bun.SQL`'s pool.** It dials every connection up to `max` when
  it has none ready, and it names no connection, so the `search_path` cache of
  ADR 0006 has nothing to attach to (ADR 0005).
- **Several connections per member.** It loses the one-connection-per-member
  invariant the `search_path` cache rests on.

## Consequences

- Measured on Bun 1.4.5 against PostgreSQL 17: a statement that fails, and a
  statement cancelled while it runs or while it waits behind another, fail
  alone; their neighbours on the connection answer. When the server
  terminates a shared connection, every statement in flight on it fails and
  none is replayed. `live` tests prove each case
  (`test/shared-operations.live.test.ts`).
- PostgreSQL cancels a backend's running statement, not a named one: a cancel
  that reaches the server just after its statement finished cancels the
  statement that runs next on that connection. Exclusive leases had the same
  race with the next operation; sharing makes a next statement more likely to
  be there. `Bun.SQL` drops a cancelled statement that has not started
  without asking the server.
- A slow statement delays the statements queued behind it on its connection,
  until the stall time stops new joins. An operation joins only once every
  member is in use, so a pool below `poolSize` behaves as before.
- A lock wait can hold a transaction back: when a statement on a shared member
  waits on a row lock that an open transaction holds, and that transaction
  awaits a pool query that joined the same member in the stall window, the
  two wait on each other. PostgreSQL does not see the wait in the client
  queue, so only a timeout ends it: `statementTimeoutMs`, 30 s by default, or
  a `lock_timeout`. `Bun.SQL`'s own pool has the same property. A workload
  that turns the statement timeout off and queries the pool from inside a
  transaction should set a `lock_timeout`.
- A raw `BEGIN`, `SET` or `RESET` sent through a datasource client, outside
  `reserve()` and `begin()`, now affects the statements running at the same
  time on its connection, not only the next ones (ADR 0006, Consequences).
- Datasources that alternate on one physical pool share less: a member is
  shared under one path at a time.
