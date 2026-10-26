# ADR 0003 — The dev executable serves `compose` and `qualify`

- **Status**: accepted
- **Date**: 2026-10-25
- **Amended**: 2026-10-25, §2 and §4: the launch reader also reads the output
  flags `--verbose`, `--quiet` and `--no-color` from the job context, and each
  member's serve run takes them. Differences (a), (b) and (d) no longer hold:
  putnami keeps the exit code of an interactive command that exits by itself
  after a signal, so a composition a signal stopped exits 0; qualify passes
  the members' serve logs through in a verbose human run; and the job context
  carries each project's resolved `runsWith` in `ProjectRef.runsWith`
  (`go.putnami.dev/protocol/job`), so a TypeScript `package.json`
  `putnami.runsWith` is read. One narrower difference replaces (b), as (f).
- **Scope**: `@putnami/dev` (`tooling/dev`): its manifest, its runtime, the
  packages `internal/compose`, `internal/qualify` and `internal/cliargs`, and
  its Go module
- **Supersedes in part**: [ADR 0002](0002-the-dev-extension-serves-version-get-and-tag.md)
  §1, on the manifest keeping CLI contract 11
- **Decisions**: D-006 and D-007 of the root `decisions.json`;
  [ADR 0079](../../../cli/doc/adr/0079-compose-and-qualify-move-to-putnami-dev.md)
  and [ADR 0080](../../../cli/doc/adr/0080-compose-requests-each-member-as-a-serve-run.md)
  of `@putnami/cli`;
  [ADR 0013](../../../../protocols/extension/doc/adr/0013-a-default-that-takes-positionals-requires-contract-15.md)
  of the extension protocol

## Context

ADR 0079 of `@putnami/cli` moves `putnami compose` and `putnami qualify` out
of the core. Both name no language, and every language extension depends on
`@putnami/dev`, so the dev extension serves them. Since ADR 0080, a
composition starts each member as a serve run of the putnami CLI, so the
commands need no engine and no scheduler: they need the workspace membership,
the workspace configuration, a putnami executable to start, and the SDK's
lock, tree fingerprint and process tree.

## Decision

### 1. Two command groups at contract 15

The manifest declares `commandGroups.compose`, whose default is `up`, and
`commandGroups.qualify`, whose default is `run`. Each subcommand is
interactive and declares the required positional `project`, so
`putnami compose <project>` keeps its spelling: a default subcommand that
takes a positional needs CLI contract 15, which the manifest now claims. Each
subcommand routes to a flat command, `compose-up` or `qualify-run`, that
declares every flag the core's command took. `compose-up` declares the
`serve` workload trait and `qualify-run` the `once` workload trait; the core
reads those traits, never the names. Each command's task runs the runtime
with `compose up` or `qualify run` in the workspace root, is never cached,
and has no deadline (`timeoutMs: -1`): a composition runs until interrupted,
and qualify bounds itself with its own ready and request deadlines.

### 2. The executable reads the launch, then runs the command

`main.go` dispatches `compose up` and `qualify run` to one launch reader. It
takes the host channel out of the environment, so no serve run inherits it.
It reads the output mode, the output flags `--verbose`, `--quiet` and
`--no-color`, and the workspace membership (`workspaceProjects`, whose
entries carry the resolved `runsWith`) from the job context putnami appends, and the workspace configuration from
`putnami.workspace.json`. It reads the absolute putnami executable from
`PUTNAMI_CLI_EXECUTABLE`. A launch without a job context, a membership or an
absolute executable fails with exit status 1 before anything starts.
`internal/cliargs` reads the forwarded positionals and flags; an undeclared
flag, a missing project or an invalid value is a usage error that exits 2.

### 3. Each member is a serve run of that executable

`internal/compose` starts each member with `<putnami> serve --projects
<member> --port 0 --serve-env - --output=jsonl`, as ADR 0080 settles, and
`internal/qualify` composes a local target the same way. The packages keep
the core's lease, proxy, database and contract code, its error codes and its
documents. `go.putnami.dev/protocol/qualify`, `http-routes`, `platform`,
`database`, `infra`, `clientcontract` and `workspace` become direct
requirements of this module.

### 4. The same documents and exit codes, with known differences

The start, exit, failure and verdict documents keep the bytes of the core's
commands. `tooling/cli/internal/cli/e2e/devworkload` replays the documents the
last core that served both commands recorded, through the core's dispatch of
the shipped manifest. qualify exits 0 for a passed verdict and 1 for every
other state; compose exits 0 only when a signal stopped it and its cleanup
was clean, and 1 otherwise.

Known differences from the core's commands:

- ~~**(a) A signal exit.** putnami reports a composition a signal stopped as
  interrupted, with exit status 130, where the core exited 0.~~ Struck by the
  2026-10-25 amendment.
- ~~**(b) Global flags.** `--verbose`, `--quiet` and `--no-color` do not reach
  the members' serve runs. qualify drops the members' serve logs in every
  output mode.~~ Struck by the 2026-10-25 amendment.
- **(c) The serve command.** A member runs `serve`, the verb this extension
  declares, so the codes `compose.no_serve_command` and
  `compose.ambiguous_serve` are deleted. `disable.jobs` turns a member off
  with `serve` or `<extension>:serve`.
- ~~**(d) `runsWith`.** Compose reads `runsWith` from each project's
  `putnami.json` in the job context. A TypeScript `package.json`
  `putnami.runsWith` is not read.~~ Struck by the 2026-10-25 amendment.
- **(e) Workspace warnings.** A result envelope carries no
  `workspaceWarnings`.
- **(f) A failed preparation.** Outside a verbose human run, qualify writes on
  stderr the member, the step that failed its serve run's preparation and
  that step's own output, where the core wrote a count of the failed steps.

### 5. A hosted run refuses both groups

A hosted run (`--credential-fd`) refuses every extension command group
before anything starts, so it refuses `putnami qualify <project> --target
<url>`. A CI job that is not a hosted run qualifies a deployment.

## Consequences

- A workspace that loads a language extension keeps `compose` and `qualify`.
  A workspace without `@putnami/dev` loses them, and the core answers with
  the reference of commands that left the core.
- The feature `tooling/workload-qualification` and its specification live
  here, and its checks are proven by this module's tests.
- An agent cannot start either command through MCP `run_jobs`, and no
  portable run carries them: both are interactive subcommands, and
  `compose-up` declares the serve workload.
