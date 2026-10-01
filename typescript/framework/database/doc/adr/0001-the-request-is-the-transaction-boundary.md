# ADR 0001 — The request is the transaction boundary, and it opens lazily

- **Status**: accepted
- **Scope**: `@putnami/database` (`typescript/framework/database`)

## Context

A handler that owns its transaction can forget to wrap two writes, or return
early without closing one; both are invisible until the data is wrong. Putting
the boundary at the request removes that choice, but a naive boundary reserves a
pooled connection and issues `BEGIN` for every read-only request, which exhausts
the pool under read load.

Repositories may be resolved from the container or built with `new`, and a
handler may still call `runInTransaction()`. Two owners of one transaction means
an inner commit can publish work the outer boundary meant to roll back. A driver
error carries the statement and its bound parameters, so quoting it in a rollback
record can leak a secret into logs.

## Decision

1. **The request owns the boundary; the first write opens it.** Transactional
   mode only flags the ambient context. No connection is reserved and no `BEGIN`
   is issued until a repository writes. Reads before the first write use the
   pool.
2. **One committer.** The request-scoped `UnitOfWork` is the sole finalizer. It
   opens the transaction, so a nested `runInTransaction()` joins it instead of
   committing. It latches after the first `commit()`/`rollback()` and delegates
   only while a transaction is active, so finalizing after a timeout is a no-op.
3. **A transaction has a deadline.** `timeoutMs` rolls back exactly once. The
   timeout and a racing commit claim finalization through one flag; a losing
   commit awaits the in-flight rollback before failing.
4. **Multi-datasource is best-effort.** One datasource is atomic. Across several,
   each `COMMIT` is attempted, every connection is released, and failures are
   raised, as an `AggregateError` when more than one failed. This is not
   two-phase commit; code that needs cross-datasource atomicity must not span
   datasources in one unit.
5. **Cancellation reaches the database.** The ambient abort signal makes a query
   that starts after it fired raise `QueryAbortError`; an in-flight query is
   cancelled at the PostgreSQL protocol level.
6. **A rollback cause is a code, never a message.** The boundary record and the
   `sql.tx.rollback.<cause>` counter carry a SQLSTATE, a class, or a fixed
   sentinel (`timeout`, `explicit-rollback`). Telemetry never reads error text,
   bound parameters, or row data, and a rollback record carries no error object.

## Rejected alternatives

- **Handler-owned transactions.** The bug is a missing wrapper, which review
  cannot see.
- **Open the transaction at request start.** Every read reserves a connection;
  pool exhaustion is a worse outage than the one atomicity prevents.
- **A `UnitOfWork` connection map.** Two owners means two commit paths, and a
  `new`'d repository joins neither reliably.
- **Driver error as rollback cause.** It copies bound secrets into a log pipeline.
- **Claim two-phase commit.** Pooled connections without a transaction manager
  cannot keep that promise.

## Consequences

- `setRollbackOnly()` is the explicit way to roll back while returning success.
- A read before the first write and a repeat after it can see different
  snapshots; write first or read through the transaction connection.
- `withTransaction()` outside `runInContext()` fails loudly.
- Operators do not see the failing statement in telemetry; it stays in the
  database's own logs under its access control.
- The shared cross-language conformance corpus asserts this behavior, so a change
  the Go adapter does not make fails the corpus.
