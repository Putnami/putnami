# Parallel

`go.putnami.dev/parallel` provides bounded fan-out helpers for running work concurrently. It encapsulates the "spawn N goroutines but cap how many run at once and bail on first error" pattern that otherwise gets hand-rolled — and frequently misrolled — in every feature.

The package uses only the standard library. It has zero external dependencies.

## Import

```go
import "go.putnami.dev/parallel"
```

## API

There are two functions, both generic over the item and (for `MapBounded`) result type:

```go
func MapBounded[T, R any](
    ctx context.Context,
    items []T,
    limit int,
    fn func(ctx context.Context, item T) (R, error),
) ([]R, error)

func EachBounded[T any](
    ctx context.Context,
    items []T,
    limit int,
    fn func(ctx context.Context, item T) error,
) error
```

Use `MapBounded` when each item produces a result you need; `EachBounded` is a shorthand for side-effect-only work.

## Semantics

### Ordered results

Results are returned in input order — `results[i]` always corresponds to `items[i]`, regardless of which goroutine finished first.

```go
results, _ := parallel.MapBounded(ctx, []int{1, 2, 3, 4}, 2, func(_ context.Context, i int) (int, error) {
    return i * i, nil
})
// results == []int{1, 4, 9, 16}
```

### Bounded concurrency

At most `limit` goroutines run `fn` at the same time. The remaining items wait until a slot frees up.

```go
parallel.MapBounded(ctx, items, 8, fn) // at most 8 in flight
```

Pass `limit <= 0` for unbounded fan-out — one goroutine per item. This is occasionally what you want (e.g., when the items list is bounded by some other resource), but usually you should set a real limit. Because dispatch itself is unbounded, every goroutine may already have been created before a sibling reports an error.

### First error wins, siblings get cancelled

When `fn` returns a non-nil error, `MapBounded`:

1. Records that error as the return value.
2. Cancels the `ctx` passed to every other in-flight worker.
3. With a positive limit, stops launching workers when the dispatcher observes cancellation.
4. With `limit <= 0`, keeps an already-created goroutine from invoking `fn` when cancellation is visible at its pre-invocation check.

Cancellation can race the pre-invocation check, so a worker may still enter `fn` with an already-cancelled `ctx`. Workers are expected to honor that context and return promptly. The error returned from `MapBounded` is the first error that `fn` produced; later errors from sibling workers (including `context.Canceled` they observe from the group cancel) are discarded.

```go
results, err := parallel.MapBounded(ctx, items, 8, func(ctx context.Context, item Item) (Result, error) {
    select {
    case <-ctx.Done():
        return Result{}, ctx.Err()
    case res := <-doSlowWork(ctx, item):
        return res, nil
    }
})
if err != nil {
    // err is whichever fn call errored first; siblings already cancelled.
}
```

This is the main reason to reach for `parallel` over a hand-rolled `sync.WaitGroup`: hand-rolled fan-out usually waits for every sibling to finish even after the answer is doomed, burning DB connections / RPCs unnecessarily.

### Pre-cancelled context

If `ctx` is already cancelled when `MapBounded` is called, it returns `(nil, ctx.Err())` without invoking `fn`.

```go
ctx, cancel := context.WithCancel(parent)
cancel()
_, err := parallel.MapBounded(ctx, items, 8, fn) // err == context.Canceled, fn never called
```

### Empty items

An empty or nil `items` slice returns `(nil, nil)` (or `nil` for `EachBounded`) without invoking `fn`.

### Context values

The `ctx` passed to `fn` is a child of the input `ctx`, so it carries all values from the parent. Workers can read them as usual.

## Typical use cases

### Parallel DB fetches with bounded connections

```go
// Resolve N config paths in parallel, but never hold more than 8 DB connections.
values, err := parallel.MapBounded(ctx, paths, 8, func(ctx context.Context, path string) (Value, error) {
    return repo.Get(ctx, path)
})
```

If the database returns an error for one path, every other in-flight `repo.Get` sees ctx cancellation and can return early, freeing the connection.

### KMS / RPC fan-out

```go
// Resolve N secrets in parallel; each call triggers a KMS RPC.
secrets, err := parallel.MapBounded(ctx, paths, 4, func(ctx context.Context, path string) ([]byte, error) {
    return secrets.Resolve(ctx, path)
})
```

A low `limit` is important here — running 200 concurrent KMS RPCs is rarely what anyone wants.

### Side-effect work

```go
// Delete a batch in parallel; stop on first error.
err := parallel.EachBounded(ctx, keys, 8, func(ctx context.Context, key string) error {
    return store.Delete(ctx, key)
})
```

## Why no `golang.org/x/sync/errgroup`?

The Putnami framework avoids external dependencies in its core helpers. The bounded fan-out pattern is roughly 30 lines of standard library code — pulling in a separate module for it has more cost than benefit. `parallel.MapBounded` uses `sync.WaitGroup`, a buffered channel as a semaphore, and `sync/atomic.Pointer` for first-error capture. The behavior is equivalent to `errgroup.WithContext` + `SetLimit`, but the result handling and ordering are built in.

## Edge cases and gotchas

- **Worker panics are recovered.** If `fn` panics, `MapBounded`/`EachBounded` recover the panic in the worker goroutine and return it as a `*parallel.PanicError` (carrying the recovered value and stack). The panic counts as that worker's error — siblings are cancelled and the process keeps running, instead of the panic terminating the program. Use `errors.As` to detect it.
- **Workers should honor `ctx`.** A worker that ignores the cancelled `ctx` will keep running until it returns naturally. `MapBounded` waits for every started worker to finish before returning, so a misbehaving worker will delay the return on first error.
- **Unbounded dispatch can create every goroutine before cancellation.** The pre-invocation check skips user work when cancellation is already visible, but it does not undo goroutine creation or eliminate the race described above.
- **Limit larger than `len(items)` is fine.** Extra slots just go unused.
- **`MapBounded` does not retry.** If transient errors are expected, retry inside `fn`.

## Contract and compatibility

See the [bounded parallel work specification](../specs/bounded-parallel-work.json),
[ordered fan-out ADR](adr/0001-ordered-all-or-nothing-fanout.md), and [support
evidence](../README.md#support-and-contract). The package is stable and
maintained, with the workspace's [pre-1.0 migration
policy](../../../../RELEASE.md), not strict compatibility between every `0.x`
minor.
