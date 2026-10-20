# ADR 0075 — An interactive command knows the directory it was typed in

- **Status**: proposed
- **Scope**: `@putnami/cli` (`internal/cli`, `internal/jobs/scheduler`),
  `@putnami/cli-model` (`jobs.ScheduledJob`), `go.putnami.dev/protocol/job`
  (`invocationDir`), the extension SDK (`context.ResolvePath`)
- **Relates to**: [ADR 0051](0051-user-scope-extensions-run-without-a-workspace.md),
  whose decision 4 this ADR leaves unchanged

## Context

An interactive extension command runs in the directory its task names, for
example `{workspaceRoot}`. Run from `docs/`, `putnami change language text
notes.md` read `<workspace>/notes.md`, while the core command it replaced read
`docs/notes.md`. The core passed the caller's directory only to a job that
runs outside a workspace, as `userScope.callerDir` and `PUTNAMI_CALLER_DIR`.
Every extension command that leaves the core and takes a file argument has the
gap.

ADR 0051 gave `userScope` and `PUTNAMI_CALLER_DIR` one meaning: the job runs
outside any workspace. The protocol, the SDK and `07-extensions.md` document
that absence means "a workspace", and the runner strips an inherited
`PUTNAMI_CALLER_DIR` for that reason. Setting them inside a workspace would
break every reader of that signal.

## Decision

1. **The job context carries an optional v2 member `invocationDir`: the
   absolute directory the user typed an interactive extension command in. The
   environment mirrors it as `PUTNAMI_INVOCATION_DIR`.** The CLI sets it on
   every interactive launch, inside a workspace and outside one. Outside a
   workspace it equals `userScope.callerDir`.
2. **`userScope` and `PUTNAMI_CALLER_DIR` keep their meaning.** Their presence
   still says that no workspace exists. `invocationDir` says nothing about
   workspaces.
3. **No planned job carries it.** `ScheduledJob.InvocationDir` is run state set
   only by the interactive launch, like `userScope` and `selection`. Nothing
   derived from it reaches a cache key or a session record. The runner strips
   an inherited `PUTNAMI_INVOCATION_DIR` from every job's environment before it
   adds its own, so a nested planned run never reads the outer command's
   directory.
4. **The member is additive.** A version 1 document may not carry it
   (`job.unexpected_field`); a present member is an absolute path
   (`job.invalid_value`), and a null or empty one is not read as absent. An
   older reader ignores it.
5. **The extension SDK offers `context.ResolvePath(ctx, p)`.** It returns an
   empty or absolute path unchanged, joins a relative path to `InvocationDir`,
   then to `UserScope.CallerDir`, and otherwise returns the path unchanged so
   it resolves against the process directory as before. It reads no file and no
   environment variable.
6. **The CLI does not change the working directory of a task.** A task that
   declares `cwd` keeps it. An extension command opts in to the user's
   directory for the path arguments it resolves, and only for those.

## Rejected alternatives

- **Set `userScope` and `PUTNAMI_CALLER_DIR` for every interactive launch.**
  It redefines "present means no workspace" and needs a new signal for that
  fact.
- **An environment variable with no context member.** The protocol documents
  each `PUTNAMI_*` variable as the mirror of a context member.
- **Run the task in the caller's directory.** It breaks every command that
  relies on the root as its working directory, such as `change finalize`.

## Consequences

- An extension reads one member to resolve a path the way the user meant it.
- The path flags of `change verify` and `change finalize` stay as documented:
  absolute, or relative to the workspace root.
- A command converts argument by argument. A command that does not call
  `ResolvePath` behaves as before.
