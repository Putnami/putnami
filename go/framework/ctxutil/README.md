# Ctxutil

`go.putnami.dev/ctxutil` holds small, dependency-free `context` helpers shared across the Putnami Go framework's request I/O paths. Its single entry point is `WithRequestTimeout`, which packages the "detached context + bounded deadline" pattern as one call. The package uses the standard library only.

## Why

Control-plane I/O often needs to outlive one caller's cancellation while still
remaining bounded. Centralizing the pattern gives those paths one invariant:
if a context is detached with `context.WithoutCancel`, immediately install an
explicit deadline before starting I/O.

## API

```go
func WithRequestTimeout(
    ctx context.Context,
    timeout time.Duration,
) (context.Context, context.CancelFunc)
```

| Behavior                | Detail                                                                    |
| ----------------------- | ------------------------------------------------------------------------- |
| Positive `timeout`      | Derives a `context.WithTimeout` child of `ctx`                            |
| `timeout <= 0`          | Pass-through: returns `ctx` unchanged with a no-op cancel                 |
| Detached context        | Re-installs a deadline on a `context.WithoutCancel` context               |
| Streaming reads         | Do NOT use — cancelling on return closes the response body                |

## Example

```go
import "go.putnami.dev/ctxutil"

// Control-plane call: bound the operation, always defer cancel.
ctx, cancel := ctxutil.WithRequestTimeout(ctx, b.requestTimeout)
defer cancel()
resp, err := client.Do(req.WithContext(ctx))
```

Re-bounding a detached context (singleflight leader):

```go
// Detaching survives a waiter's cancellation but strips the request deadline;
// re-bound it immediately so a hung dependency cannot wedge the cohort.
ctx, cancel := ctxutil.WithRequestTimeout(context.WithoutCancel(reqCtx), cfg.Timeout)
defer cancel()
```

See `doc/getting-started.md` for full reference.

## Support and contract

The SDD owner is `go`. `go.putnami.dev/ctxutil` is public, documented, maintained, and classified
`stable` in the workspace [support catalog](../../../putnami.support.json).
Before v1.0.0, a minor `0.x` release may still contain a breaking change; the
[release policy](../../../RELEASE.md) requires release notes and migration
documentation rather than strict compatibility between every pre-1.0 minor.

The [bounded request contexts specification](specs/bounded-request-contexts.json)
and [detached context ADR](doc/adr/0001-detached-work-must-be-rebounded.md) define
the contract. The behavior is pinned by [package tests](ctxutil_test.go) and is
consumed by the framework's storage and security request paths.
