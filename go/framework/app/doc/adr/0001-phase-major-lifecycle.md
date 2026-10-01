# ADR 0001 — Run application lifecycle phases in dependency order

- **Status**: accepted
- **Scope**: `go.putnami.dev/app` (`go/framework/app`)

## Context

Configuration needs a built container, module coordination needs configured
plugins, and cleanup must run after every partial startup. Concurrent plugin
startup needs a deadline and one cleanup owner, so a blocked starter cannot
race its own stop method.

## Decision

Startup runs phase by phase across the whole tree: module `PreConfigure`
top-down before dependency injection; container build; plugin `Configure`
sequentially; module `PostConfigure` bottom-up; migrations and invokers; plugin
`Start` concurrently under one bounded child context; then module `OnStart`
top-down.

Shutdown reverses ownership: module `OnStop` bottom-up and in reverse hook
order, plugin `Stop` in reverse registration order, container close last.
Cleanup aggregates errors. A failed or timed-out plugin start runs the same
shutdown path. Start failures are aggregated after every starter returns. A
timeout cancels siblings and gives cooperative starters a bounded drain window.
`ListenAndServe` alone owns signal-driven stop and supplies a bounded shutdown
context.

Describe mode starts the container lazily, configures the graph, and runs
describers without migrations, invokers, starters, runners, or stoppers.

## Rejected alternatives

- **Run each module's full lifecycle before the next.** Descendants would
  configure before shared providers and sibling plugins are ready.
- **Start and stop plugins sequentially in registration order.** Independent
  listeners block each other, and forward shutdown disposes dependencies before
  their consumers.
- **Let the signal goroutine call `Stop`.** A signal during startup could stop
  a plugin while its `Start` still runs.
- **Build the eager container in describe mode.** Eager providers would open
  sockets or pools during a build-time metadata pass.

## Consequences

- Starters must honor cancellation; the framework bounds the wait but cannot
  kill a goroutine.
- `OnStop` and `Stop` must tolerate partial startup and be idempotent.
- A new lifecycle phase must define its place in startup and rollback.
- Describe contributors derive metadata from declarations and in-memory state,
  never from runtime effects.
