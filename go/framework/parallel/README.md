# Parallel

`go.putnami.dev/parallel` is a small fan-out helper for the Putnami Go framework. It runs work concurrently with a bounded worker count, cancels siblings on the first error, and returns results in input order. The package uses the standard library only — no `errgroup` dependency.

## Why

Every backend feature with parallel I/O tends to reinvent the same pattern:

```go
var wg sync.WaitGroup
results := make([]Result, len(items))
for i, item := range items {
    wg.Add(1)
    go func(i int, it Item) {
        defer wg.Done()
        results[i] = work(it)
    }(i, item)
}
wg.Wait()
```

That hand-rolled version is almost always unbounded and almost always without context cancellation on first error. `parallel.MapBounded` makes "do this in parallel, but safely" the obvious one-line choice.

## API

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

| Behavior                | Detail                                                              |
| ----------------------- | ------------------------------------------------------------------- |
| Result order            | Same as input order                                                 |
| Concurrency             | At most `limit` goroutines in flight; `limit <= 0` means unbounded |
| Error handling          | First non-nil error from `fn` is returned; others discarded         |
| Sibling cancellation    | On first error, the `ctx` passed to in-flight workers is cancelled  |
| Bounded cancellation    | Positive-limit dispatch stops when cancellation is observed         |
| Unbounded cancellation  | Created goroutines skip `fn` when cancellation is already visible   |
| Pre-cancelled ctx       | Returns `ctx.Err()` without invoking `fn`                           |
| Empty items             | Returns `(nil, nil)` / `nil` without invoking `fn`                  |
| Worker panics           | Recovered and returned as `*PanicError`; siblings cancelled         |

## Example

```go
import "go.putnami.dev/parallel"

paths := []string{"/a", "/b", "/c", "/d"}

values, err := parallel.MapBounded(ctx, paths, 8, func(ctx context.Context, path string) ([]byte, error) {
    return store.Get(ctx, path)
})
if err != nil {
    return err
}
// values[i] corresponds to paths[i]
```

For side effects only:

```go
err := parallel.EachBounded(ctx, paths, 8, func(ctx context.Context, path string) error {
    return store.Delete(ctx, path)
})
```

See `doc/getting-started.md` for full reference.

## Support and contract

The SDD owner is `go`. `go.putnami.dev/parallel` is public, documented, maintained, and classified
`stable` in the workspace [support catalog](../../../putnami.support.json).
Before v1.0.0, a minor `0.x` release may still contain a breaking change; the
[release policy](../../../RELEASE.md) requires release notes and migration
documentation rather than strict compatibility between every pre-1.0 minor.

The [bounded parallel work specification](specs/bounded-parallel-work.json) and
[ordered fan-out ADR](doc/adr/0001-ordered-all-or-nothing-fanout.md) define the
contract. [Package tests](parallel_test.go) cover ordering, limits, cancellation,
errors, and panic recovery; the task API sample exercises the helper in a real
handler.

This project is a pilot of the executable spec gate:
[`putnami.features.json`](putnami.features.json) declares acceptance checks
per requirement, each protecting test binds itself with `spectest.Proves`,
and `options.sdd.verification.specs` is `enforce` in
[`putnami.json`](putnami.json) — deleting, skipping, or renaming a declared
check fails this project's normal `test` gate. `putnami specs verify` replays
the recorded verdict.
