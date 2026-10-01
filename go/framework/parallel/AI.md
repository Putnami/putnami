# go.putnami.dev/parallel

Bounded fan-out helpers for concurrent work: ordered results, context cancellation on first error, no external dependencies.

## Quick Start

```go
import "go.putnami.dev/parallel"

// Fan out N items, at most 8 in flight, cancel siblings on first error.
results, err := parallel.MapBounded(ctx, items, 8, func(ctx context.Context, item Item) (Result, error) {
    return doWork(ctx, item)
})

// Side-effect-only variant.
err := parallel.EachBounded(ctx, items, 8, func(ctx context.Context, item Item) error {
    return process(ctx, item)
})
```

## Semantics

- Results from `MapBounded` are returned in input order, matching `items`.
- `limit <= 0` means unbounded (one goroutine per item).
- The first non-nil error returned by `fn` is the result; remaining workers receive a cancelled context.
- With a positive limit, dispatch stops when cancellation is observed. With `limit <= 0`, goroutines may already exist for every item, but each skips `fn` when cancellation is visible before invocation.
- Pre-cancelled `ctx` short-circuits: returns `ctx.Err()` without invoking `fn`.
- Empty `items` returns `(nil, nil)` (or `nil` for `EachBounded`).
- A panic in `fn` is recovered and returned as a `*parallel.PanicError`; it cancels siblings like any other error instead of crashing the process.

## When to use

Reach for `parallel` instead of hand-rolling `sync.WaitGroup` + per-item goroutines whenever you want bounded concurrency and "stop the rest on first error" — typical cases are parallel DB lookups, KMS calls, or HTTP fan-out where unbounded goroutines burn connections.

See `doc/getting-started.md` for full reference.

## Maintained contract

This public package is `stable` in the workspace [support
catalog](../../../putnami.support.json). Its behavior is defined by the [bounded
parallel work specification](specs/bounded-parallel-work.json) and [ordered
fan-out ADR](doc/adr/0001-ordered-all-or-nothing-fanout.md). Before v1.0, follow
the workspace [migration-based compatibility policy](../../../RELEASE.md); do
not infer strict compatibility between every `0.x` minor.
