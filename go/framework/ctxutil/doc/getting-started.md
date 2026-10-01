# Ctxutil

`go.putnami.dev/ctxutil` provides small, dependency-free `context` helpers for the Putnami Go framework's request I/O paths. Its single entry point, `WithRequestTimeout`, packages the "detached context + bounded deadline" pattern that otherwise gets hand-rolled — and occasionally misrolled — in every backend.

The package uses only the standard library. It has zero external dependencies.

## Import

```go
import "go.putnami.dev/ctxutil"
```

## API

```go
func WithRequestTimeout(
    ctx context.Context,
    timeout time.Duration,
) (context.Context, context.CancelFunc)
```

`WithRequestTimeout` bounds one control-plane / non-streaming operation with a per-operation deadline derived from the caller's context.

## Semantics

### Positive timeout derives a deadline

A positive `timeout` returns `context.WithTimeout(ctx, timeout)` — a child context that is done `timeout` from now (or when the parent is done, whichever comes first).

```go
ctx, cancel := ctxutil.WithRequestTimeout(ctx, 5*time.Second)
defer cancel()
resp, err := client.Do(req.WithContext(ctx))
```

### Non-positive timeout is a pass-through

A zero or negative `timeout` returns the caller's context unchanged, with a no-op `cancel`. Callers can therefore always `defer cancel()` unconditionally, regardless of the configured timeout.

```go
ctx, cancel := ctxutil.WithRequestTimeout(ctx, 0) // ctx unchanged
defer cancel()                                    // no-op, safe
```

### Re-bounding a detached context

The wedge this helper exists to prevent: a background operation is sometimes run on a context detached from the caller's cancellation via `context.WithoutCancel`, so one waiter cancelling does not abort the call a whole cohort shares (e.g. a singleflight leader). But detaching also **strips the request deadline** — the detached context has no deadline at all. A hung dependency running on it would then block every waiter forever.

`WithRequestTimeout` re-installs a bounded deadline immediately after detaching, restoring fail-closed behavior:

```go
// Leader runs on a cancellation-detached context so a waiter's cancel does not
// abort the shared call — but re-bound it at once so a hung resolver fails
// closed instead of wedging the whole cohort.
ctx, cancel := ctxutil.WithRequestTimeout(context.WithoutCancel(reqCtx), cfg.Timeout)
defer cancel()
raw, status, err := doUpstreamCall(ctx, ...)
```

The audit `security-timeout` guard flags any `context.WithoutCancel(...)` in framework code that is captured but not re-bounded on a following line — this is the shape it expects.

## When NOT to use

Streaming reads. A streaming `Get` returns a body the caller reads after the function returns; cancelling the derived context on return (via `defer cancel()`) would close that body mid-download. Such reads must stay bounded only by the caller's own context, not a per-operation deadline installed here.

## Typical use cases

### Control-plane backend calls

```go
// Bound a non-streaming metadata / delete / list call.
ctx, cancel := ctxutil.WithRequestTimeout(ctx, b.requestTimeout)
defer cancel()
return b.client.Delete(ctx, bucket, key)
```

### Singleflight leaders / detached background writes

```go
// Re-bound the detached context so a hung upstream fails closed.
ctx, cancel := ctxutil.WithRequestTimeout(context.WithoutCancel(reqCtx), cfg.Timeout)
defer cancel()
```

## Why not a bespoke wrapper per package?

A shared helper keeps zero-value behavior, detachment, and cancellation
consistent across framework backends. It also gives the audit guard one
mechanical shape to verify.

## Contract and compatibility

See the [bounded request contexts
specification](../specs/bounded-request-contexts.json), [detached context
ADR](adr/0001-detached-work-must-be-rebounded.md), and [support
evidence](../README.md#support-and-contract). The package is stable and
maintained, with the workspace's [pre-1.0 migration
policy](../../../../RELEASE.md), not strict compatibility between every `0.x`
minor.
