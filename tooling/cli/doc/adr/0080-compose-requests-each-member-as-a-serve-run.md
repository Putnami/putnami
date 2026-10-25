# ADR 0080 — Compose requests each member as a serve run

- **Status**: accepted
- **Date**: 2026-10-25
- **Amended**: 2026-10-25, by
  [ADR 0079](0079-compose-and-qualify-move-to-putnami-dev.md): the packages and
  their goldens live in `@putnami/dev` (`tooling/dev/internal/compose` and
  `tooling/dev/internal/qualify`), a member runs `serve`, and the codes
  `no_serve_command` and `ambiguous_serve` are deleted
- **Scope**: `@putnami/cli` (`internal/compose`, `internal/qualify`,
  `internal/commands/composecmd`, `internal/commands/qualifycmd`,
  `internal/engine`, `internal/jobs/scheduler`), feature
  `cli/workload-qualification`

## Context

`putnami compose` started its members in two halves. One engine run executed
the finite steps of every member's serve pipeline (config merge, generate,
describe) with `Request.WithholdServeSteps`, and returned each member's serve
step on `SessionResult.Withheld` instead of starting it. Compose then started
each serve step itself, through `scheduler.RunJobWithProcessEnv`, and ran the
watched target under its own watch session, which reran only the serve step.

So two code paths started a serve step: the engine's, for `putnami serve`, and
compose's. The engine carried a stage that only compose used, and compose
depended on the engine and the scheduler. Compose cannot move out of the core
while it starts jobs itself.

`putnami serve --serve-env -` now carries what compose needs from a serve run.
It reads the members' environment from stdin and gives it to the serve steps
only. Its stream keeps every readiness claim and names each serve step's
process group and its leader's start time.

## Decision

1. **Each member is one serve run of the CLI.** Compose starts each member, in
   start order, as
   `<CLI> serve --projects <member> --port 0 [--no-watch] --serve-env - --output=jsonl`,
   where `<CLI>` is the executable of the running CLI. Only a watched target
   omits `--no-watch`. Stdin carries the member's environment document, stdout
   carries the session stream, and stderr carries the serve logs, which compose
   passes through to its own stderr. Each run records its own session.
2. **Only the engine starts a serve step.** The serve run plans the member's
   pipeline, runs its finite steps with the cache, starts the serve step and,
   for a watched target, replans the pipeline after a change. Compose plans no
   job and starts no job.
3. **Compose reads the stream.** The `process` event names the serve step and
   its process group. A readiness claim counts only from that step. A claim
   with a port moves the member's proxy. A `workload` claim marks startup
   complete. The serve step's `task:end` empties the proxy. `--ready-timeout`
   and `readyMs` count from the first `process` event, as they counted from
   the serve step's start before: the finite steps of the run stay bounded by
   their own task timeouts.
4. **A run that ends before its serve step is `prepare_failed`.** A usage error
   of the nested run, a failed finite step, and an exit before the serve step
   spawns fail the start with `compose.prepare_failed` and the tail of the
   run's output. A serve step that ends before its first claim stays
   `member_exited`.
5. **The lease records both groups of each member.** The lease records the
   serve run's process group and each serve step's group, each with its
   leader's start time. The next composition reaps both when a composition dies
   without its teardown.
6. **Teardown stops the members in reverse start order.** Compose sends
   SIGTERM to each serve run and waits for it, then closes the proxies, drops
   the databases and releases the lease.
7. **The core deletes the coupling.** It deletes `Request.WithholdServeSteps`,
   `SessionResult.Withheld`, `SessionResult.Workspace`,
   `engine/serve_withhold.go`, `scheduler.RunJobWithProcessEnv`,
   `scheduler.WithProcessGroupObserver` and compose's `Options.Preparation`.

## Rejected alternatives

- **Keep withholding the serve step.** The engine would keep a stage with one
  caller, and compose would keep its own job runner and its own watch loop.
- **A planning call to `@putnami/dev`, made by the core, that returns the
  members to serve.** The proxies and the databases live for the whole
  composition, so a supervisor process is still needed, and the core would need
  a rule that feeds one live job's output into another job's environment.
- **Host RPC methods that prepare a session, start a job and stop a job.** The
  core would expose job control to extensions for one consumer.
- **One serve run for every member.** One run cannot start the members one
  after another, each after the previous one's first claim, and cannot give
  each member a different environment.

## Consequences

- The start, exit, status and verdict documents keep their bytes. The goldens
  under `internal/compose/testdata/serve-run` and
  `internal/qualify/testdata/local-verdict` pin them through the built CLI.
- A composition records one session per member, not one preparation session.
  No session file holds the environment document.
- Each member costs one more CLI start and workspace load. A cached finite step
  still hits the cache.
- A watched target replans its serve pipeline after a change, as
  `putnami serve` does, instead of rerunning only its serve step.
- `no_serve_command` and `ambiguous_serve` come from the commands whose traits
  declare `workload: serve`. A project that no extension serves fails in its
  serve run, as `prepare_failed`. A serve task whose manifest `env` declares
  `CONFIG_DATA` is refused by the serve run, as `prepare_failed`.
- A serve run that does not stop within 30 seconds is killed, and the teardown
  report names it. A serve step it orphaned stays on the lease for the next
  reap.
- Compose no longer imports `internal/engine` or `internal/watch`, which lets it
  move to `@putnami/dev` with `qualify`.
