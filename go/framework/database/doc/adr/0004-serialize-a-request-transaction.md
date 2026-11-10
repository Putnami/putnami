# ADR 0004 — Serialize the queries of a request on its unit-of-work transaction

- **Status**: accepted
- **Scope**: `go.putnami.dev/database` (`go/framework/database`); amends the
  unit-of-work part of [ADR 0001](0001-pool-scoped-transaction-boundaries.md)

## Context

A request unit of work holds one `pgx.Tx` per enrolled pool, so one
connection. A pgx connection serves one query at a time and is not safe for
concurrent use. A handler that fans out reads (`errgroup`,
`go.putnami.dev/parallel`) sends them on the request context, so every
goroutine resolves the same transaction: the second query fails with
`conn busy`, and the race can leave the connection unable to finish the
transaction at the boundary. The unit of work promised safe concurrent use by
the goroutines of one request; its mutex guarded only its own maps.

## Decision

The unit of work wraps each transaction it begins in a gate that admits one
operation at a time:

- An operation holds the transaction from the moment it is sent until the
  connection is free again: `Exec`, `CopyFrom`, `Prepare` and a savepoint
  `Begin`, `Commit` or `Rollback` for their call; `Query` until its rows are
  closed or drained; `SendBatch` until its results are closed. `QueryRow`
  sends its statement when the row is scanned and holds the transaction for
  that `Scan`, so a row never scanned never holds it.
- A query waiting for its turn waits under its own context. When the context
  ends first, the query fails with a `db.transaction` error that says another
  query of the request holds the transaction until its result is closed.
- The request boundary closes the gate first, so a later query fails with a
  `db.transaction` error. It closes a result the request left open, waits for
  an operation still running under the boundary's own context, then commits or
  rolls back. When that context ends first, commit fails and rollback runs
  anyway, which closes the busy connection.

Queries of one request on one datasource therefore run one after the other
and all see the request's uncommitted writes. `LargeObjects` and `Conn` reach
the connection directly and stay outside the gate.

`database.Autocommit(ctx)` gives a request real parallel reads: a query on the
returned context takes its own pooled connection and commits on its own,
outside the unit of work and outside any `WithTx` transaction the context
carries. Such a read does not see the request's uncommitted writes, and a
write through it commits whatever the request's outcome. `WithTx` under it
begins and owns its own transaction.

## Rejected alternatives

- **Fail a second concurrent query fast with a clear error.** Concurrent reads
  would keep failing, and the documented promise would be withdrawn.
- **Buffer every result inside a unit of work** so no caller code runs while
  the transaction is held. It loads whole result sets into memory and
  re-implements pgx row decoding.
- **Gate the transactions `WithTx` owns.** `WithTx` never promised concurrent
  use of its callback's transaction; its panic rollback must not wait on a
  result the panicking goroutine left open.

## Consequences

- A query issued while the same goroutine still holds open rows on the same
  pool waits until its context ends, where it used to fail at once with
  `conn busy`. Close or drain the rows first, or read the nested result
  through `Autocommit`.
- Inside a unit of work, concurrent queries on one datasource take turns; only
  `Autocommit` reads, or queries on different datasources, run in parallel.
