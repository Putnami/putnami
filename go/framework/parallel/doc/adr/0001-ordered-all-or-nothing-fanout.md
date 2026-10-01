# ADR 0001 — Bounded fan-out returns ordered results or one failure

- **Status**: accepted
- **Scope**: `go.putnami.dev/parallel` (`go/framework/parallel`)

## Context

Callers fan out independent work under a database, RPC, or KMS concurrency
limit. Completion order is random, but results must align with the input. A
failure must tell siblings to return. A panic in a worker goroutine escapes any
`defer` in the caller goroutine.

## Decision

`MapBounded` writes each successful result at its input index. A positive
limit bounds in-flight workers with a semaphore; a non-positive limit starts
one goroutine per item. Workers get a child context of the caller's.

The first error wins atomically, cancels the child context, waits for started
workers, and returns no partial results. With a positive limit, dispatch stops
once cancellation is observed. With a non-positive limit, each goroutine checks
the child context just before calling the function and skips the call when it
is canceled; a racing cancellation can still enter the function with a
canceled context. A worker panic is recovered in its goroutine and becomes a
`PanicError` with the value and stack, on the same first-error path.
`EachBounded` is the result-free form of the same code.

## Rejected alternatives

- **Completion-order results.** Every caller would re-sort.
- **Partial results with an error.** Callers could consume an incomplete set;
  partial success needs an explicit result type.
- **Launch all goroutines and limit inside workers.** The bounded path would
  still create unbounded scheduler and memory pressure.
- **Let worker panics crash the process.** They bypass the caller's recovery.

## Consequences

- Started workers must honor cancellation; the helper cannot kill them.
- Which simultaneous error wins is not input-ordered.
- A non-positive limit is unbounded and must not take untrusted or very large
  input.
- Side-effecting work must tolerate a sibling failing after some effects
  complete.
