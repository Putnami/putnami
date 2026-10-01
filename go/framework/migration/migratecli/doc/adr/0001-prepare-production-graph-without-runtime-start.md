# ADR 0001 — Prepare the production graph without starting runtime workloads

- **Status**: accepted
- **Scope**: `go.putnami.dev/migratecli` (`go/framework/migration/migratecli`)

## Context

A migration binary with hand-built wiring drifts from the service. Starting
the application is unsafe for a one-shot job, because listeners, consumers,
and invokers can run before the schema changes. Preparation can still build
eager resources, so every exit must close the container.

## Decision

The service exposes one `AppBuilder` shared by the server and migrate
binaries. Every subcommand calls `Application.Prepare`, reads the per-app
`migration.Registry`, and never calls `Start`, `Migrate`, or `ListenAndServe`.
`up` forces runner application; everything else stays in the registry and
runner contracts.

SIGINT or SIGTERM cancels the command context. Cleanup is registered before
preparation, uses a fresh background context, and runs on every exit,
including a failed preparation phase. It calls `Stop`, then closes any
container attached during preparation, because `Prepare` does not mark the
application running. Exit codes: 0 success, 1 user input, 2 operational
failure, 3 drift. Structured output keeps completed records visible when a
later runner fails.

## Rejected alternatives

- **Separate migration wiring.** It can omit a feature or target another
  datasource.
- **Start, then stop listeners.** Runtime work races the migration.
- **Rely on `Stop` alone.** A prepared application is not running, so its
  container needs an explicit close.
- **Drift as operational failure.** Automation could not tell drift from an
  unavailable database.

## Consequences

- `inspect` runs no migration-state operation but can reach a dependency an
  eager plugin opens in `Configure`.
- Plugins in the migration binary keep `Configure` repeatable and runtime
  effects in `Start`.
- A cleanup error never replaces the command's exit result.
