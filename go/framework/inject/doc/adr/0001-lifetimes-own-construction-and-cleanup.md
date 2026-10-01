# ADR 0001 — Dependency lifetimes own construction and cleanup together

- **Status**: accepted
- **Scope**: `go.putnami.dev/inject` (`go/framework/inject`)

## Context

Caching a value in one container and registering its cleanup in another leaks
resources or closes shared ones early. Eager startup can build several
singletons before a later factory fails. Concurrent first resolution must share
one build without turning a cross-goroutine cycle into a deadlock.

## Decision

The container that owns a singleton's provider constructs, caches, and closes
it. The requesting child scope constructs, caches, finalizes, and closes a
scoped value; resolving a scoped provider from its declaring root is a scope
violation. Children close before parents. Each container runs close hooks in
reverse registration order, only for values it built.

`ContainerContext.Start` validates requirements, dependencies, cycles, and
scope edges, then resolves every non-lazy singleton. On any failure, built
singletons run their close hooks, caches clear, and the context returns to
idle so startup can be retried. Concurrent builders share one in-flight
result, and a wait graph detects cycles before a goroutine blocks.

## Rejected alternatives

- **Cache every value in the provider's container.** Scoped values would be
  shared between requests.
- **Run every registered close hook.** An unresolved lazy provider holds
  nothing to dispose.
- **Mark a failed start closed.** Callers could not fix configuration and
  retry.
- **Per-goroutine resolution stacks for cycles.** Two goroutines can wait on
  each other without either stack holding the whole cycle.

## Consequences

- Factories using explicit tokens declare dependencies with `WithDeps`;
  constructor registrations derive them.
- Scope boundaries (an HTTP request, event, or job) own `Finalize` and
  `Close`.
- Close hooks must survive a rollback followed by a later start and close.
- Lazy providers defer construction failures and resource acquisition to first
  resolution.
