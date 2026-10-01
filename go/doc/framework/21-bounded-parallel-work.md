# Bounded parallel work

`go.putnami.dev/parallel` runs independent work concurrently while preserving
input order and bounding resource use.

```go
results, err := parallel.MapBounded(ctx, items, 8,
    func(ctx context.Context, item Item) (Result, error) {
        return process(ctx, item)
    },
)
```

`MapBounded` returns results in the same order as `items`. A positive `limit`
caps workers; `limit <= 0` allows one worker per item. `EachBounded` provides
the same behavior for side effects without a result slice.

## Failure behavior

The first non-nil worker error wins, sibling contexts are cancelled, and no
partial result slice is returned. With a positive limit, the dispatcher stops
launching workers when it observes cancellation. With `limit <= 0`, a goroutine
may already have been created for every item; each checks cancellation before
invoking the worker and skips the call when cancellation is already visible. A
cancellation racing that check can still enter the worker, so invoked workers
must cooperate with their context. The function waits for every goroutine it
already started. A worker panic is recovered as `*parallel.PanicError` and
follows the same cancellation path instead of terminating the process.

An already-cancelled context returns `ctx.Err()` without invoking the worker;
empty input returns successfully without invoking it.

## Support and compatibility

`go.putnami.dev/parallel` is a public, documented, maintained package classified
`stable`. Its bounded-parallel specification and accepted ordered fan-out ADR
live next to the package source. Before v1.0.0, minor `0.x` releases may still
contain documented breaking changes; strict compatibility across every pre-1.0
minor is not promised.
