# ADR 0079 — Compose and qualify move to @putnami/dev

- **Status**: proposed
- **Date**: 2026-10-25
- **Scope**: `@putnami/cli` (`internal/cli`, `internal/commandmeta`,
  `internal/engine`, `internal/mcp`), `@putnami/dev`,
  `go.putnami.dev/protocol/runner` and `go.putnami.dev/protocol/telemetry`;
  the feature `cli/workload-qualification`, which becomes
  `tooling/workload-qualification`
- **Amends**: [ADR 0073](0073-the-core-declares-no-development-verb.md), the
  consequence that keeps the `compose` and `qualify` literals;
  [ADR 0039](0039-derived-smoke-contracts-and-fail-closed-verdicts.md), its
  scope; [ADR 0080](0080-compose-requests-each-member-as-a-serve-run.md), its
  scope and the consequence on `no_serve_command` and `ambiguous_serve`;
  decision 3 of
  [runner ADR 0006](../../../../protocols/runner/doc/adr/0006-commit-addressed-engine-planned-request.md)
- **Decisions**: D-006 and D-007 of the root `decisions.json`;
  [ADR 0013 of the extension protocol](../../../../protocols/extension/doc/adr/0013-a-default-that-takes-positionals-requires-contract-15.md)
  and [ADR 0002 of `@putnami/dev`](../../../dev/doc/adr/0002-the-dev-extension-serves-version-get-and-tag.md)
  set the precedents this one follows

## Context

`putnami compose` and `putnami qualify` were built-in commands of the core.
They name no language and no toolchain, but they compose workloads, reap
leases, provision databases and run HTTP smoke contracts: none of that is
the core's job of planning and running tasks. Since
[ADR 0080](0080-compose-requests-each-member-as-a-serve-run.md), compose
starts each member as a serve run of the CLI, so it imports neither the
engine nor the scheduler, and can run outside the core.

Four places in the core and the runner protocol refused the two commands by
name: `runner.UnportableCommands`, the portable shape check of
`internal/engine`, the `--no-watch` ownership check of `internal/cli`, and
the `longLivedCommands` table of MCP `run_jobs`. ADR 0073 left them because
the names were core commands then.

## Decision

1. **`@putnami/dev` serves both commands.** Its manifest declares the command
   groups `compose`, whose default is `up`, and `qualify`, whose default is
   `run`. Each subcommand is interactive and declares the required positional
   `project`, so `putnami compose <project>` and `putnami qualify <project>`
   keep their spelling, and the manifest claims CLI contract 15. Each
   subcommand runs one flat command, `compose-up` or `qualify-run`, which
   declares the flags the core command took. Its task runs the dev executable
   in the workspace root, uncached and with `timeoutMs: -1`, so only the
   interrupt or the verdict ends it.
2. **Traits replace the four name reads.** `compose-up` declares the `serve`
   workload and `qualify-run` the `once` workload. The portable refusal on the
   submitting and the executing side reads the workload trait, the MCP
   `run_jobs` refusal reads the `serve` workload and the
   interactive-carried rule, and `--no-watch` belongs to a serve-workload
   command, flat or through the group that runs it. The runner protocol
   refuses no command by name: a commit-addressed request is not malformed
   for a command it names, and the executing engine refuses it by its traits.
3. **The executable reads what the core held.** The dev executable reads the
   workspace membership from the job context's `workspaceProjects` and the
   workspace configuration from `putnami.workspace.json`. It starts each
   member's serve run with the absolute putnami executable that
   `PUTNAMI_CLI_EXECUTABLE` names, and fails before anything starts without
   it. The lock, the tree fingerprint and the process tree come from the SDK
   packages the core already used.
4. **The core deletes the commands.** It deletes `internal/compose`,
   `internal/qualify`, `internal/commands/composecmd` and
   `internal/commands/qualifycmd`, their catalog entries and registrations,
   `parsePositiveDuration`, and the four name reads. `compose` and `qualify`
   join the removed core roots: without an extension that declares them, the
   CLI refuses `putnami compose …`, `putnami qualify …` and `putnami help` of
   each with exit status 2 before the workspace bootstrap. No alias or
   transition command remains.
5. **A hosted run refuses both.** A hosted run (`--credential-fd`) refuses
   every extension command group before anything starts
   (`ErrHostedExtensionCommand`), so it refuses `putnami qualify <project>
   --target <url>`, which the core ran there. A CI job that is not a hosted
   run qualifies a deployment.
6. **The documents keep their bytes.** A parity test in `@putnami/cli` replays
   the start, exit, failure and verdict documents the last core that served
   both commands recorded, through the real dispatch of the shipped manifest.
   The telemetry vocabulary names `compose`, `compose-up`, `qualify` and
   `qualify-run`.

### Known differences from the core's commands

- **(a) A signal exit.** A composition stopped by a signal exits 130, where
  the core exited 0: the core reports every interactive command it canceled
  as interrupted.
- **(b) Global flags.** `--verbose`, `--quiet` and `--no-color` no longer
  reach the members' serve logs. `qualify` drops the members' serve logs in
  every output mode, where the core printed them with `--verbose`.
- **(c) The serve command.** Compose requests `serve`, the development verb
  `@putnami/dev` declares, so `compose.no_serve_command` and
  `compose.ambiguous_serve` are deleted. A member that `disable.jobs` turns
  off names `serve` or `<extension>:serve`.
- **(d) `runsWith`.** Compose reads each project's `runsWith` from its
  `putnami.json`, which the job context carries. A `putnami.runsWith` that
  only a TypeScript `package.json` declares, which the core read from the
  language extension's project view, is not read.
- **(e) Workspace warnings.** The dev executable's result envelopes carry no
  `workspaceWarnings`.

## Rejected alternatives

- **Keep the commands in the core.** The core would keep two subsystems that
  plan nothing and schedule nothing, against D-006.
- **A new extension for the two commands.** Every language extension already
  depends on `@putnami/dev`, so it reaches every workspace that can serve a
  workload; another extension would need its own installation path.
- **Keep the name reads as a fallback.** A name read would refuse a command
  that another extension declares under the same name with other traits, and
  the owner rule allows no transition shim.
- **A host RPC method that answers the resolved workspace.** It would carry the
  provider-view `runsWith` of difference (d), but it adds a host contract for
  one field. An additive member of the job context is the smaller follow-up.

## Consequences

- The internal package ceiling of `@putnami/cli` falls from 78 to 74, and the
  module no longer requires `go.putnami.dev/protocol/qualify`,
  `http-routes` or `platform`.
- The schema of a version 2 execution request loses its `allOf` that refused
  `compose` and `qualify`: a relaxation of the protocol, whose executing engine
  still refuses both commands by their traits.
- A workspace that loads an older `@putnami/dev`, without the groups, answers
  that no extension declares `compose`; `putnami extensions update` brings the
  groups.
- The flat commands `compose-up` and `qualify-run` activate only on a project
  that holds `putnami.extension.json`; users run the groups.
- The feature `tooling/workload-qualification`, its specification and its
  documentation live in `tooling/dev`, with the requirement and check ids of
  `cli/workload-qualification`, and one more requirement for the manifest and
  the dispatch.
