# go.putnami.dev/ctxutil

Small, dependency-free context helpers for the Putnami Go framework's request I/O paths. Single entry point: `WithRequestTimeout` — the "detached context + bounded deadline" pattern as one call.

## Quick Start

```go
import "go.putnami.dev/ctxutil"

// Bound a control-plane / non-streaming operation with a per-operation deadline.
ctx, cancel := ctxutil.WithRequestTimeout(ctx, b.requestTimeout)
defer cancel()
resp, err := client.Do(req.WithContext(ctx))
```

## Semantics

- A zero or negative `timeout` is a pass-through: the same context is returned with a no-op cancel, so callers can always `defer cancel()` unconditionally.
- A positive `timeout` derives a `context.WithTimeout` child of the caller's context.
- It is the single source of truth for the framework's per-operation request deadline.

## When to use

- Any control-plane / non-streaming backend I/O that wants a bounded per-operation deadline.
- Re-bounding a **detached** context: one built with `context.WithoutCancel` to survive a waiter's cancellation (e.g. a singleflight leader) MUST be re-bounded immediately with `WithRequestTimeout`, because detaching also strips the request deadline and would otherwise let a hung dependency wedge every waiter forever.

## When NOT to use

Streaming reads. Cancelling the derived context on return closes the returned body, so a streaming `Get` must stay bounded only by the caller's context.

See `doc/getting-started.md` for full reference.

## Maintained contract

This public package is `stable` in the workspace [support
catalog](../../../putnami.support.json). Its behavior is defined by the [bounded
request contexts specification](specs/bounded-request-contexts.json) and
[detached context ADR](doc/adr/0001-detached-work-must-be-rebounded.md). Before
v1.0, follow the workspace [migration-based compatibility
policy](../../../RELEASE.md); do not infer strict compatibility between every
`0.x` minor.
