# ADR 0002 — A provisioned secret lives in exactly one artifact

- **Status**: accepted
- **Scope**: `putnami-extension-sdk` (`tooling/extension-sdk`)

## Context

A test run that needs a database hands a connection string to the test process.
Every obvious channel outlives the run. The event stream is written to a session
log that `putnami report` reads back. Process arguments are visible to anything
that lists processes. A declared task output is cached, restored into other
worktrees and possibly uploaded. An environment variable on a shared parent
leaks to every sibling process.

A run killed with `SIGKILL` never runs its cleanup, so its container survives
untracked, and the next run collides with it or starts another.

## Decision

The provisioned credential exists in exactly one place: an invocation-scoped
output declared `sensitive`. Consumers read it through `BindingFrom`; nothing
else carries it. The sensitive artifact is not a cacheable output and cannot be
restored into another run.

Beside it, a non-secret lease records what was provisioned. A later run reads
it, recognizes an orphan of a `SIGKILL`ed invocation, and reaps it. Teardown is
a `finally` finalizer, so it runs on the failure path too.

Mode selection fails closed. `auto` provisions when it can and skips when it
cannot; `require` fails rather than running tests against nothing; a closure
that declares no database provisions nothing. An externally supplied binding
always wins over a provisioned one.

## Invariants

- The provisioning job's event stream contains no credential; its tests assert
  this over the full stream.
- The credential appears in no process argv.
- The sensitive artifact is invocation-scoped and never cached.
- Teardown runs on the failure path.
- The lease carries no secret.
- An external binding takes precedence over a provisioned one.
- `require` fails rather than proceeding without a database.

## Rejected alternatives

- **The DSN in the test task's environment.** Inherited by every descendant and
  by anything that dumps the environment on failure.
- **A normal task output.** A cached secret has an unbounded lifetime.
- **Log it at debug level.** Session logs are artifacts; "debug only" is a
  setting, not a boundary.
- **Deferred cleanup alone.** `SIGKILL` skips it.
- **Redact in the emitter.** Redaction must be correct everywhere forever; not
  emitting is correct once.

## Consequences

- A consumer must read the artifact. There is no convenience environment
  variable, and adding one would undo this decision.
- A task that wants a database needs a writable invocation scope, so it cannot
  be a pure cached task.
- Orphan reaping is best-effort: a machine that loses the lease file keeps the
  container.
