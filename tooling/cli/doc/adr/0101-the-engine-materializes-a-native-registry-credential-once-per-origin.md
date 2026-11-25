# ADR 0101 — The engine materializes a native registry credential once per origin

- **Status**: accepted
- **Date**: 2026-11-25
- **Amends**: [registry ADR 0001](../../../../protocols/registry/doc/adr/0001-host-keyed-credential-seam.md)
  (one guarantee of `--materialize`)
- **Holds**: [ADR 0055](0055-run-credentials-stay-out-of-repository-processes.md)
- **Scope**: `@putnami/cli` (`internal/jobs/scheduler`), the task member
  `nativeRegistryCredential` and the job variable
  `PUTNAMI_NATIVE_REGISTRY_CREDENTIALS` (`go.putnami.dev/protocol/extension`),
  `registry.NativeCredentialValidity` and `registry.SeamForRunFlag`
  (`go.putnami.dev/protocol/registry`),
  `go.putnami.dev/sdk/extension/registrycred`, `@putnami/go`, `@putnami/cloud`
  (`cloud registry-token --materialize`)

## Context

A job of `@putnami/go` that may fetch a module asks `@putnami/cloud` to write
the `.netrc` entry of the declared module origin before it runs `go`:
`putnami cloud registry-token --host <origin> --materialize`
(`registrycred.EnsureNativeCredential`). The SDK remembers the answer for the
life of the process, and a job process runs one job, so every such job starts
one CLI.

Measured on 2026-11-24, a no-cache gate of this repository started
507 CLI processes from jobs, which spent 406,192 ms of CPU: 9.6 % of the run's
CPU and the largest single part of its orchestration cost. Every one of them
wrote the same entry for the same host. The Go fixture of the orchestration
counts starts 5 of them for 9 jobs.

## Decision

1. **A task declares that it needs a native registry credential.** Its
   manifest member `nativeRegistryCredential` names a registry of the
   project's effective `registries` and the member that holds its endpoint:
   `{"registry": "go", "endpoint": "origin"}`. The engine reads that member as
   a URL or a bare host and takes its lowercase host name. An absent entry, an
   absent member or an empty value declares nothing. The member is an
   execution fact, like the endpoints themselves: it enters no cache key and no
   task contract digest.
2. **The engine calls the same seam, once per host, before the first job that
   needs it.** Before it starts a job whose task declares a host, and before
   the job's deadline starts, the engine runs
   `putnami cloud registry-token --host <host> --materialize --for-run` with
   its own executable, in the workspace root, unless it already holds an
   answer for that host that is still valid. The child gets the engine's
   environment less the variables that name this run's own spawns, which a
   job does not inherit either: a bound request, the provider choice, the
   caller and invocation directories, the cache socket, the host RPC channel,
   the job credential descriptor and the native credential answers. It keeps
   the bound commit a job loses: it runs in the same checkout for the commit
   the engine bound, and the job it starts loses it as any job does. Jobs that need the same host wait for the
   one call in flight, which a canceled run does not cut short: the seam's
   own bound, 10 seconds and 1 more for the child's pipes, is how long it
   waits for it. Planning, cache-key computation and a job found in the
   cache call nothing.
3. **An answer holds for `registry.NativeCredentialValidity`, 4 minutes from
   the start of the call.** The seam states no expiry, and returning one would
   break it (registry ADR 0001), so the engine states the validity itself. A
   job that starts after it ends renews the answer once, and the jobs waiting
   for that host share the renewal. Every answer holds for the same window: a
   signed-out user, or a machine without a cloud, is told once per window,
   not once per job.
4. **The engine tells the job what it holds.** Each job that needs a host
   receives `PUTNAMI_NATIVE_REGISTRY_CREDENTIALS`: a JSON object keyed by
   host, each value the instant the answer stops holding (`validUntil`, an RFC
   3339 UTC instant), the outcome (`materialized`, `cloud-absent`
   or `not-signed-in`; the protocol also spells `unsupported` and `failed`,
   which the engine does not hand over) and the line the SDK would have
   logged for it, which a job logs at debug level or holds as a warning. The variable names no
   credential: the token stays in the file the cloud wrote. The engine removes
   an inherited or declared value before it sets its own. It names no host
   whose call was refused as `unsupported`, which is what a cloud that
   predates `--for-run` answers, or `failed`, which the next job may not be:
   those jobs call the seam themselves, as before.
5. **`EnsureNativeCredential` starts no CLI while that answer holds.** For a
   host the variable names, before `validUntil`, it returns the engine's
   outcome and logs its line once per process, as it does today. A host it
   does not name, an answer that no longer holds, and a process without the
   variable call the seam themselves, as before: outside a run, in a job the
   engine of an older CLI starts, and when a job resolves another host than
   the engine (`GO_REGISTRY_URL` set for the job).
6. **`--for-run` asks for a credential that stays valid for the window.**
   The seam gains one flag, `--for-run`, beside `--materialize`.
   `@putnami/cloud` then reuses a stored registry token only when it stays
   valid for `registry.NativeCredentialValidity` and 30 seconds more, and
   mints a fresh one otherwise. A fresh token that would not outlive the
   window either fails the call, which the engine then does not hand over.
   Every other shape keeps its 30-second margin.
   A cloud that predates the flag refuses it with exit 2, so the engine never
   tells a job that an older cloud's credential holds for the window.
7. **ADR 0055 holds.** A hosted run (`--credential-fd`, or a run whose jobs
   run offline) calls nothing: the engine skips the call, and the jobs skip it
   as before. No daemon and no socket: the call is the same bounded child the
   job started, now started by the engine. The engine records it as
   repository code (ADR 0055 §4), as it records `putnami cloud release-set`,
   before it starts it.

## Rejected alternatives

- **Keep the call per job and memoize it on disk.** A file that says a
  credential was written is state shared by every worktree and run, which the
  next run would have to trust without knowing who wrote it.
- **A daemon or socket that serves the credential to jobs.** Registry ADR
  0001 rejected it, and one per worktree does not fit many worktrees.
- **Return the expiry on the seam.** Registry ADR 0001 makes any metadata on
  its stdout a breaking change for every deployed cloud. A flag the engine
  sends is additive: an older cloud refuses it, and the engine falls back.
- **Hand the bearer to jobs.** A token in a job's environment is what ADR
  0055 keeps out of repository processes; `go` reads `.netrc` anyway.
- **Materialize every declared origin at the start of the run.** A run that
  executes no Go job, or finds every one in the cache, would start a CLI for
  nothing.

## Consequences

- A run starts one CLI per declared origin and per 4-minute window in which a
  job needs it, instead of one per job. The orchestration count "CLI starts per
  job" of the Go fixture is 0; the engine's ledger counts its own call.
- A job that runs longer than the window keeps the credential it started with,
  as a job that refreshed it itself did.
- With a cloud older than decision 6, the engine starts one CLI per window
  that answers `unsupported`, and every job starts its own, as before.
- An extension built on an older SDK ignores the variable and starts its own
  CLI, as before. An older engine sets no variable, and a job on this SDK
  behaves as before.
- A task that declares the member in a workspace without that registry entry
  costs nothing.
