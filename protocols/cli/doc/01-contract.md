# The Putnami CLI contract

This document defines the contract every Putnami CLI surface implements. It is
a stable, machine-facing contract: scripts, CI, and agents depend on it, so the
values below must not drift.

## Exit codes

| Code | Name | Meaning |
|------|------|---------|
| `0` | Success | The command completed successfully. |
| `1` | Failure | A job/build/test failed, or an unexpected internal error occurred. |
| `2` | Usage | Invalid invocation: bad flags or arguments, an unknown command, a selector that matched nothing, or a contract/config validation error. |
| `3` | Auth | Authentication or authorization failed. |
| `4` | API | A remote or upstream API call (registry, cloud) failed. |
| `130` | Signal | The process was interrupted by a signal (SIGINT/SIGTERM). |

A run stopped by a signal reports `130`, not `0` and not `1`. Its remaining
work was killed before it could pass or fail, so the run has no verdict: it is
neither a success a gate may act on nor a failure attributable to the code
under test.

Producers do not hard-code these numbers. They return an error tagged with a
class sentinel (`ErrUsage`, `ErrAuth`, `ErrAPI`, `ErrSignal`, `ErrNotFound`, …)
and the dispatcher maps it to a code via `ExitCodeForError`. `ErrNotFound`,
`ErrNoMatch` and `ErrInvalidConfig` all fold into `Usage`.

## Output modes

`--output=<mode>` selects how a command renders. `--json` is a shorthand alias
for `--output=json`; combining `--json` with a different `--output` is a usage
error.

| Mode | Meaning |
|------|---------|
| _(auto)_ | Chosen from the environment: `cloud-logging` under `K_SERVICE`, a live/text renderer on a TTY, else `jsonl`. |
| `text` | Human-readable output (spinners/progress on a TTY). |
| `json` | A single aggregated JSON result object, written once. |
| `jsonl` | A stream of line-delimited JSON events for an executed run, or one terminal `plan:end` record for a plan-only preview. |
| `cloud-logging` | Structured JSON for Google Cloud Logging. |

`json` and `jsonl` are both "structured" (`OutputMode.IsStructured`). A command
that produces one result object (e.g. `projects list`, `config show`) treats
them identically. Only streaming **job** commands (`build`, `publish`,
`deploy`, …) distinguish the two: `jsonl` streams per-job events, while `json`
buffers and emits one aggregated result.

For a one-result command, the single document is always a `ResultV2` envelope.
Older commands whose implementation assembled their payload as JSONL records
preserve those records, in order, in the envelope's `data` member; they do not
escape as unversioned documents on stdout.

## Result envelope

In `--output=json`, a command emits one [`ResultV2`](../result_v2.go) object.
**Every machine document carries `protocolVersion: 2`; a document without the
member is version 1, and no shipping surface emits one.** The full v2 contract —
all six documents, the typed task identity, the settled verdict rules and the
v1→v2 migration — is [doc/02-result-v2.md](02-result-v2.md).

```json
{
  "protocolVersion": 2,
  "command": "contracts check",
  "status": "success",
  "data": { "outcome": "clean", "manifest": "putnami.extension.json" },
  "exitCode": 0
}
```

A job-running command reports the typed `run` summary instead of stuffing a
tally into `data`:

```json
{
  "protocolVersion": 2,
  "command": "build",
  "status": "success",
  "run": {
    "outcome": "success",
    "exitCode": 0,
    "counts": { "total": 3, "succeeded": 3, "failed": 0, "canceled": 0, "skipped": 0 },
    "reuse": { "localCache": 1, "remoteCache": 0, "coalesced": 1 },
    "durationMs": 812
  },
  "exitCode": 0
}
```

A successful `--plan` or plan-only `--dry-run` reports `plan` instead of
`run`; planning completed, but no session executed:

```json
{
  "protocolVersion": 2,
  "command": "deploy",
  "status": "success",
  "exitCode": 0,
  "plan": {
    "dryRun": true,
    "metrics": { "tasks": 1, "edges": 0, "projects": 1, "byCommand": { "deploy": 1 } },
    "tasks": [
      {
        "identity": {
          "key": "/services/api:deploy",
          "scope": "project",
          "project": { "id": "/services/api", "name": "api" },
          "task": { "name": "deploy", "command": "deploy", "kind": "cloud-deploy" },
          "provider": { "extension": "@putnami/cloud" }
        },
        "cache": false
      }
    ]
  }
}
```

`plan` requires `status: "success"` and `exitCode: 0`, and is mutually
exclusive with `run`, `data`, and `error`. An empty preview is still a complete
document (`metrics.tasks: 0`, `tasks: []`), never an empty stdout stream or a
synthetic zero-task run. The JSONL projection is one `plan:end` record carrying
the same `plan`; it has no `session:end`, `run`, or session artifact.

On failure, `status` is `"failure"`, `exitCode` carries the taxonomy code, and
`error` is populated:

```json
{
  "protocolVersion": 2,
  "command": "cloud status",
  "status": "failure",
  "error": {
    "code": "auth",
    "message": "token expired",
    "next": "putnami auth login"
  },
  "exitCode": 3
}
```

`error.code` is one of `usage | auth | api | signal | failure`, matching the
exit-code taxonomy. `status` has **three** values in v2 — `success | failure |
aborted` — and an interrupted command reports `aborted` with `error.code:
"signal"` and `exitCode: 130`, so the envelope agrees with the code the process
itself returns instead of folding the abort into a plain failure. When
available, `error.next` gives a suggested recovery command or documentation
link.

The shape is published as [`schemas/result-v2.json`](../schemas/result-v2.json)
(`$id` `https://putnami.dev/schemas/putnami-cli-result-v2.json`) for non-Go
surfaces. The version-1 envelope — [`Result`](../result.go) and
[`schemas/result.json`](../schemas/result.json) — is retained as a READ-ONLY
shape for documents produced by CLI builds published before the removal: its
constructor and writer are deleted, so nothing in this repository can emit one.

## Reserved global flags

A fixed set of global flags is part of the contract: they mean the same thing on
every surface and an extension must not define a command flag that shadows one.
[`ReservedGlobalFlags`](../flags.go) is the single source of truth.

| Flag | Short | Meaning |
|------|-------|---------|
| `--output` | | Output mode (`text \| json \| jsonl \| cloud-logging`). |
| `--json` | | Shorthand for `--output=json`. |
| `--help` | `-h` | Show help. |
| `--version` | `-V` | Print the CLI version. |
| `--verbose` | `-v` | Verbose output. |
| `--debug` | `-d` | Debug output (implies `--verbose`). |
| `--quiet` | | Suppress non-essential output. |
| `--color` / `--no-color` | | Force / disable colored output. |

Framework-specific orchestration flags (project selection, cache control,
parallelism, …) are deliberately **not** reserved globals — they are private to
the framework CLI, not part of the cross-surface contract.
`IsReservedGlobalFlag` recognizes any of a flag's forms (`--output`,
`--output=json`, `-v`, `--no-color`); `ValidateNoReservedShadow` returns a
usage-classified error when an extension flag collides with a reserved global.

## Contract version

[`CurrentContract`](../contract.go) is the integer version of this contract:
the base every extension is stamped with. [`LatestContract`](../contract.go) is
the highest contract this CLI reads. Contract `5` (`AgentContentContract`) is
**additive**: it adds the extension `agentContent` contribution, and only a
manifest that declares one is stamped `5`. Every other manifest keeps contract
`4` and loads exactly as before, while a contract-4 CLI — whose permissive
decode would drop the contribution silently — refuses a contract-5 package as
written for a newer putnami.
Contract `8` (`ExtensionDependenciesContract`) is additive for
`extensionDependencies` that name a registry extension (`@scope/name`): from 8
on, the CLI installs, pins and loads each one. A reader below 8 would load the
extension without its dependencies, so it refuses the stamp. The floor binds a
manifest loaded from the artifact store or from a path the workspace declares.
The Go archive packager stamps 8, even on a manifest that declares only hooks
and dependencies; the npm packager leaves the list out of its stamp.
Contract `9` (`VerbVocabularyContract`) is additive for the `verbs` section:
the verbs an extension declares, with their descriptions, aliases, default
traits and related commands. From 9 on, help, completion and alias resolution
read the loaded vocabularies first, and a job whose command declares no traits
takes its verb's default traits. A reader below 9 would plan those jobs with
other traits, so it refuses the stamp. The floor binds a manifest wherever it
loads from. Only a manifest that declares the section is stamped 9.
Contract `10` (`SourceBoundCacheContract`) is additive for the
`cache.sourceBound` task policy. From 10 on, the CLI stores and looks up the
task's entry at a key derived from its cache key and the source binding of the
task's own project, so an entry serves only at the binding its outputs embed.
A reader below 10 stores the entry at the cache key alone and would replay
outputs that embed a stale binding, so it refuses the stamp. The floor binds a manifest wherever it loads from. Only a manifest
with a task that declares the field is stamped 10.
Contract `11` (`CommandKindContract`) is additive for four command traits and
two batch policy fields. A command's or a verb's traits may declare `kind`, what
the host reads from the command's results (`verification`, `criteria`,
`diagnostics` or `files`); `workload`, how the command runs (`serve` or
`once`); `rewritesSources`; and `consumesReleaseSet`. A batchable task may
declare `batchable.kind` (`verification` or `diagnostics`) and
`batchable.phase`, the tally and the phase name of its shared invocation. Strict
validation refuses a value outside each vocabulary. A reader below 11 would
plan a declared kind or workload as a plain command, so it refuses the stamp.
The floor binds a manifest wherever it loads from. Only a manifest that
declares one of the fields is stamped 11.
Contract `12` (`PublishChannelsContract`) is additive for
`declares.publishChannels`, the publish channels a publish task serves. From 12
on, a publishing run counts a project's declared publish channel as served only
by a planned job whose task declares that channel. A reader below 12 would
judge those channels by another rule, so it refuses the stamp. The floor binds
a manifest wherever it loads from. Only a manifest with a task that declares
the field is stamped 12.
Contract `13` (`ProvisionedToolchainContract`) is additive for the
`provisioned` candidate of a runtime toolchain. From 13 on, the CLI downloads,
verifies and unpacks the host platform's archive of each toolchain the
workspace lock pins, below the Putnami home, and a `provisioned` candidate
resolves to the executable the lock's archive names in that tree. The
candidate names no path, so a reader below 13 would not know where the
executable is, and it provisions nothing, so it refuses the stamp. The floor
binds a manifest wherever it loads from. Only a manifest whose runtime
toolchain declares the candidate is stamped 13.
Contract `14` (`InputSelectorsContract`) is for the input selectors an
extension expands: `runtime.inputSelectors`, and a task file pattern that
names one. From 14 on, the CLI asks the extension that declares a task for the
files each selector reads, through the workspace protocol's input-resolve
call, and expands no selector itself. A reader below 14 would expand a
selector by another rule or not at all, so it refuses the stamp. The floor
binds a manifest wherever it loads from. Only a manifest that lists or names a
selector is stamped 14. The rung retires rung 6: a manifest stamped below 14
that names a selector is refused, and its remedy is a build stamped 14
(`putnami extensions update`).
Contract `15` (`DefaultPositionalsContract`) is additive for a command
group's default subcommand that declares positionals. From 15 on, a word after
the group that names none of its subcommands runs the default, with the word as
the default's first argument; a declared subcommand wins over a word of the
same name. A reader below 15 answers "unknown subcommand" for that word, so it
refuses the stamp. The floor binds a manifest wherever it loads from. Only a
manifest whose group default declares positionals is stamped 15; positionals
on another subcommand keep the base stamp.
Contract `16` (`ChangedFilesContract`) is additive for the run's changed
files as a step fact. A step may gate on them (`activation.changedFiles`: the
step is planned only when a changed file the target project owns matches one
of the globs) and bind them (`with: {"from": "context", "path":
"selection.changedFiles"}`: the project-relative changed files the project
owns, on a gated step only those its globs select, bound into the step's
parameters and so into its cache key). A run that
measured no diff (`--all`, `--projects`) plans no gated step and binds no
value. A reader below 16 ignores the gate and resolves no context binding, so
it would plan the step on every run and key it without the files; it refuses
the stamp. The floor binds a manifest wherever it loads from. Only a manifest
with a step that declares either form is stamped 16.
Contract `17` (`HostHooksContract`) is additive for the `hostHooks` member:
the hooks an extension asks the agent hosts to run. From 17 on, an explicit
`putnami install`, `init` or `upgrade` writes them into `.claude/settings.json`
and `.codex/hooks.json` for a workspace that opts into the extension's agent
content and binds the collaboration contract the hooks require. A reader below
17 would install the extension without its hooks, so it refuses the stamp. The
floor binds a manifest wherever it loads from. Only a manifest that declares
the member is stamped 17.
Contract `6` was additive for `go-embed:build` and `go-embed:test` in an
extension's project task inputs, which the CLI expanded itself. Contract 14
retires that meaning; the number stays taken.
Contract `4` adds dependent-owned `sessionPrerequisites`: their same-session
command expansion, selection and parameter projection, and functional gate
edges are semantic. A contract-3 CLI has no representation for that field, so
contract-4 manifests must be re-packaged and cannot load in either direction
across the boundary. Contract `3` is the vNext core: a contract-3 extension speaks the v3 task
contract (tasks declare their outputs, effects and source mutation), job context
v2 (`protocolVersion: 2` with a typed task identity), runtime event protocol v2,
and lock format v2 — the four move together. Contract `2` adds
extension-contributed MCP tool descriptors and their subprocess call protocol.
Contract `1` covers the reserved global-flag registry above, the
`--output`/`--json` semantics, and the exit-code taxonomy. Contract `0` is the
pre-registry world: manifests without a `cliContract` field.

An extension manifest's `cliContract` field is **earned, not claimed**: the
package/publish job validates the staged manifest strictly and stamps the
lowest contract whose vocabulary covers it (`extension.RequiredCLIContract`) on
success, so a stamped contract is a verified one. (The Go authoring SDK's `manifest.Builder` stamps too — it runs
the identical gate, which is what earning the stamp means.) Loaders never assume
the CLI and an extension come from the same release train — they negotiate:

| Manifest contract vs CLI | Loader behavior |
|--------------------------|-----------------|
| absent (0) or **below** what its vocabulary requires | Fail loud: "declares CLI contract N but this putnami requires M". The remedy names the side that has to move: re-package the extension, or `putnami extensions update`. |
| from its required contract up to **`LatestContract`** | Enforce strictly: a reserved shadow is a hard error — the stamp claimed compliance. |
| **above `LatestContract`** | Fail loud: "extension requires a newer putnami (contract N > M)". A future contract is never half-interpreted. |

One manifest is outside the ladder: a manifest that declares **no contract
surface** — no commands, no command groups, no tools, no agent content, e.g. a framework package
shipping only a `preBuild` hook. The packager deliberately leaves it unstamped
because there is nothing for the contract to govern, so the loader must not
demand a stamp from it. Loader and packager share the predicate
(`extension.DeclaresContractSurface`).

Rule for bumps: every increment MUST list what changed in the changelog block
on the constant. Up to contract 2 it also had to ship adaptation for contract
N-1; contract 3 retired that rule together with the adaptation itself, so a
non-additive increment now ships a MIGRATION (a command that moves a workspace
or an extension onto the new contract) instead of a silent downgrade. An
additive increment ships none: a manifest that does not use its vocabulary
keeps the stamp it already carries.

The ladder above is one row of the repository's compatibility budget,
which states the supported version window, the migration and the user-facing
remedy for every public artifact format:
[Compatibility and Migration](../../../tooling/cli/doc/21-compatibility-and-migration.md)
([ADR 0010](../../../tooling/cli/doc/adr/0010-compatibility-budget.md)).
