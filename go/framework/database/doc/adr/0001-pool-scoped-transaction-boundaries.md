# ADR 0001 — Scope transactions to pools and finalize them at one boundary

- **Status**: accepted
- **Scope**: `go.putnami.dev/database` (`go/framework/database`)

## Context

Repositories join an enclosing transaction without taking a transaction
parameter on every method. A service may use several pools, and a request may
fail, panic or be cancelled with connections enrolled. One process-wide
transaction value would let a repository run on another datasource's
connection, and presenting several PostgreSQL transactions as one atomic unit
would hide partial commits.

## Decision

`WithTx` carries an immutable map keyed by `*Pool`. Nested work on the same
pool reuses its transaction without savepoints. Without a request unit, work
on another pool opens and owns an independent transaction. The owning boundary
commits on nil, rolls back on error, and keeps a deferred rollback for panic
and failed commit.

The request-scoped `UnitOfWork` enrolls one transaction per pool lazily and
finalizes once. A single pool is atomic. Several pools commit sequentially in
enrollment order and are not two-phase commit: a failure keeps the committed
prefix, rolls back the rest, and returns an error with bounded counts.
Finalization strips request cancellation and may apply a configured deadline.

Nesting is symmetric. A unit that enrolls a pool an outer `WithTx` carries
joins without taking ownership; `WithTx` under a request unit enrolls and binds
that unit's transaction without finalizing it. The first owner is the sole
commit and rollback owner. A joining `WithTx` returns its callback error, and
the request boundary reconciles it with the request outcome.

Transaction observations carry datasource, duration, outcome and a fixed
rollback classification, never raw error text or panic values.

## Rejected alternatives

- **One `pgx.Tx` in the context.** A foreign pool could join it.
- **Implicit savepoints for nested same-pool work.** Rollback semantics would
  depend on hidden nesting.
- **Claim multi-datasource atomicity.** A committed datasource cannot roll
  back.
- **Finalize with the cancelled request context.** Rollback could be skipped,
  leaving a pooled connection occupied.

## Consequences

- Cross-datasource atomic workflows need an outbox, a saga or another explicit
  design.
- A finalization timeout bounds cleanup, at the risk that a backend has not
  finished its rollback by the deadline.
