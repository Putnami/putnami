# ADR 0001 — Decide container lifetimes and visibility at composition time

- **Status**: accepted
- **Scope**: `@putnami/runtime` (`typescript/framework/runtime`)

## Context

A container that checks its graph on first resolution turns a missing provider
into a request-time 500 on one route and a cycle into a stack overflow. If reach
is checked at resolution time, "private" means "nobody asked yet". Async
factories let two concurrent `get()` calls both see "not constructed" and both
construct, so the container must enforce "singleton".

## Decision

Lifetime, reach, and correctness are fixed when the container is composed.

- `ContainerContext.start()` validates the whole graph before constructing
  anything: unresolvable dependencies, cycles, and module requirements no visible
  provider satisfies fail there.
- Containers form a tree and resolution walks up the parent chain only. A
  `private` provider is reachable from its declaring container and nothing above
  or beside it.
- A singleton is built at most once per container; concurrent async resolution
  shares one in-flight promise. A scoped provider is built once per scope, and
  scopes never share instances.
- `close()` runs `onClose` hooks in reverse construction order. After close,
  resolution fails with `ContainerClosedError`.
- The context is a one-shot state machine, `idle → started → closed`. Starting
  twice, closing before start, and resolving before start each raise a named
  error.
- The package root exports only the stable injection surface. Test doubles live
  in `@putnami/runtime/testing`. The escape hatches (`useContainer`, `resolve`,
  `resolveInjection`, `createScopeProxy`, `SCOPE_CONTAINER_KEY`) live in
  `@putnami/runtime/inject`.

## Rejected alternatives

- **Lazy validation.** Reports a composition error as a runtime failure of
  whichever path resolved the token first.
- **Visibility by convention.** Found only when a refactor breaks an unknown
  consumer.
- **Racing construction, keep the last.** Builds and leaks a second pool, client,
  or cache.
- **Dispose in construction order.** Consumers see closed handles during
  shutdown.
- **Escape hatches on the root barrel.** Every root symbol is a compatibility
  promise; these are framework plumbing.

## Consequences

- Composition must be complete before `start()`; `@putnami/application`
  rebuilds the context on every composition change.
- Startup cost grows with the number of registrations, paid once where a
  failure is readable.
- A scoped provider cannot be resolved outside a scope.
- A checked-in surface test pins the root barrel.
