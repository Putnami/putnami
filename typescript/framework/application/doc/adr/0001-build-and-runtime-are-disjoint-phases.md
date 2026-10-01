# ADR 0001 — Keep build generation and the runtime lifecycle disjoint

- **Status**: accepted
- **Scope**: `@putnami/application` (`typescript/framework/application`)

## Context

An application owns a module tree, a plugin list, a dependency container and
shutdown hooks, and none of them comes up alone. Some plugins write files (route
loaders, OpenAPI documents, client code, capability manifests); others open
resources (listeners, pools, subscriptions). One entry point for both makes a
build machine bind a port and a production container regenerate code.

The framework is a library, so a startup failure belongs to the caller; killing
the host process removes the embedder's or test's chance to handle it. An
orchestrator sends `SIGTERM` and then kills, so shutdown must finish on a
deadline the framework picks.

## Decision

`build()` and `start()` share no step.

`build()` runs `generate()` across the tree in parallel, waits for that barrier,
then runs `postGenerate()` so derived generators see every artifact. It emits
migration artifacts, then publishes the capability manifest and design graph
last. The capability manifest is invalidated before generation and written after
it succeeds, so a failed build never leaves bytes that appear to describe it.
`start()` never calls `generate()`.

`start()` runs in this order: plugin `warmup` sequentially in module-tree order,
migration-source collection, container construction, plugin `migrate` hooks,
concurrent plugin `start`, then the application runner. Warmup is sequential
because a plugin may register providers, contribute migration sources or
`ensurePlugin()` a dependency, and the container is built from the state after
warmup.

Composition is invalidated, never patched. `provide()`, `register()` and `use()`
drop the cached container context, and `prepare()` drops it again before
building, so a provider added after `.context` was read is still in the started
container.

`stop()` reverses ownership: shutdown hooks in reverse registration order,
plugins in reverse module-tree order, the container last. Each step is isolated:
a throwing hook or plugin is logged and the drain continues. `stop()` on an
application that is not running does nothing.

A failed `start()` unwinds exactly what it acquired: it stops only plugins whose
`start()` resolved, closes the container, leaves the application not running and
re-throws. Concurrent starters all settle before teardown; the first failure is
thrown and every other one is logged at error severity. A plugin whose `start()`
rejected is not stopped, because closing what it never opened raises a secondary
error that masks the cause.

`bootstrapServe` is the only place that ends the process. It builds the graph
inside its failure guard, so a config or dependency failure during construction
is reported like a start failure, as one structured log line, then exits
non-zero. `installSignalHandlers` alone owns signal-driven stop: one drain per
process, exit zero only when it resolves, forced non-zero exit after the
shutdown deadline.

## Rejected alternatives

- **`start()` generates missing artifacts.** The container would write files,
  need a writable filesystem and diverge from the reviewed build output.
- **Build the container at composition time.** Providers from `warmup` would be
  missing, and eager factories would open sockets during declaration.
- **Patch the cached container.** It needs a second consistency model; a rebuild
  keeps one description of the application.
- **Stop every plugin after a failed start.** The secondary error masks the
  real one.
- **Throw an aggregate error.** It changes the error callers already catch;
  logging the rest keeps them visible.
- **`Application.start()` exits on failure.** The exit decision belongs to the
  entry point.

## Consequences

- Plugin `start()` implementations run concurrently and must not depend on a
  sibling having started. Ordering belongs in `warmup`.
- `stop()` implementations tolerate partial startup and are idempotent.
- Anything a workload needs at runtime ships as a committed or packaged
  artifact.
- A new lifecycle phase states its place in both startup and rollback; adding
  one is a contract change.
- Teardown errors are logged, not propagated. A caller that must fail on a dirty
  shutdown observes it through its own hook.
