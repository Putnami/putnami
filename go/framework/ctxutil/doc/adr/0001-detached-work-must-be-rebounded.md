# ADR 0001 — Detached bounded work must receive a new deadline

- **Status**: accepted
- **Scope**: `go.putnami.dev/ctxutil` (`go/framework/ctxutil`)

## Context

Deduplicated or background work must survive cancellation of the caller that
led it, so it uses `context.WithoutCancel`. That also drops the caller's
deadline, and a remote call under it can block every waiter forever.

## Decision

`WithRequestTimeout` is the shared bound for non-streaming dependency calls. A
positive duration derives `context.WithTimeout`; zero or negative returns the
original context and a no-op cancel. Code that detaches bounded work calls it
immediately before the dependency call. The helper neither detaches nor picks
durations. The caller owns the cancel function and runs it only once the
dependency is done with the context.

## Rejected alternatives

- **One private helper per backend.** Zero-value and streaming behavior drift
  and cannot be audited as one rule.
- **Rebuild the old deadline after `WithoutCancel`.** It pretends detached work
  still has the caller's lifetime.
- **A package default timeout.** Timeout policy belongs to the operation and
  backend configuration.
- **Use it for streaming reads and cancel on return.** It can invalidate a
  response body before the caller reads it.

## Consequences

- Detaching bounded work takes two steps: detach, then re-bound explicitly.
- A non-positive duration means no bound; consumers that need one validate
  their configuration.
- Streaming APIs stay bounded by the caller context or a lifecycle the
  streaming consumer owns.
