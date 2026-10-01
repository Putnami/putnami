# Overview

The Putnami CLI (`go.putnami.dev/tooling/cli`) is the workspace orchestration CLI, written in Go. It delivers fast startup, low memory usage, and zero-dependency distribution as a single static binary.

## Design Philosophy

### Manifest-Driven Orchestration

The Go CLI never imports or executes extension code directly. It reads `putnami.extension.json` manifests declaratively and spawns job processes as subprocesses. This means:

- **No dynamic module loading** — extension discovery is deterministic.
- **Language-agnostic execution** — jobs can be implemented in any language (TypeScript, Go, Python, shell).
- **Crash isolation** — a failing job subprocess cannot take down the CLI process.

### Stdlib Only

The Go CLI has zero third-party runtime dependencies. Every capability — JSON parsing, file watching, SHA-256 hashing, HTTP, process management — uses the Go standard library, and the only modules `go.mod` requires are in-repo ones (`go.putnami.dev/protocol/*` and `go.putnami.dev/cli/model`), each `replace`d to a local path. This eliminates supply chain risk and simplifies builds: `go build` produces a single static binary.

### Contract Compatibility

The Go CLI maintains full behavioral compatibility with the TypeScript CLI:

- Same exit codes (0, 1, 2, 3, 4, 130).
- Same JSONL event format for job communication.
- Same flag names, aliases, and environment variables.
- Same cache key computation algorithm.
- Same project selection and dependency propagation semantics.

## Architecture

The CLI is two modules: `go.putnami.dev/tooling/cli` (`tooling/cli/`) and
`go.putnami.dev/cli/model` (`tooling/cli-model/`).

**`cli-model` is the data model** — `workspace`, `extension` and `jobs`, holding
type declarations and pure methods over them. It answers *what is a workspace, an
extension and a job* independently of the code that runs them. Its `go.mod`
requires only `go.putnami.dev/protocol/*`, so **it can never import the CLI**;
that one-way dependency is what keeps a renderer from dragging in the scheduler.
Anything with an effect stays in `tooling/cli`.

```
tooling/cli-model/
├── workspace/                 Workspace/project model, graph, filters, probe view, impact
├── extension/                 Extension model: manifests, commands, pipelines, contracts
└── jobs/                      Job model: plans, invocations, results, the canonical reducer

tooling/cli/
cmd/putnami/main.go            Entry point
internal/
├── cli/                       Argument parsing, routing, help, structural ratchets
├── commands/                  Twelve command verticals + shared/sharedtest
├── engine/                    Engine.Run — the single run lifecycle
├── extension/                 Extension discovery, install, integrity, registry, lockfile I/O
├── git/                       Git operations (branch, diff, merge-base)
├── hooks/                     Lifecycle hooks: extension (onInstall, preBuild) and workspace (hooks.cli, hooks.commands)
├── jobs/                      Job planning, DAG scheduling, subprocess execution, caching
├── output/                    Renderers (text, live TUI, JSONL, cloud-logging)
├── profiler/                  Chrome trace format profiling
├── store/                     Content-addressed cache store (local, remote, chained)
├── telemetry/                 Anonymous opt-in metrics
├── watch/                     File watching, change classification, serve lifecycle
├── workspace/                 Workspace detection, project discovery, probing, syncing
└── workspace_state/           Session recording and audit trails
```

Principal packages only; the full package-by-package map, including the twelve
command verticals under `internal/commands/`, is in
[Internals](13-internals.md#module-and-package-layout).

### Boot Sequence

1. **Signal context** — `main()` creates a context that cancels on SIGINT/SIGTERM for graceful shutdown.
2. **Workspace detection** — Walk upward from cwd looking for `putnami.workspace.json` or `package.json` with `workspaces`.
3. **Config loading** — Merge global (`~/.putnami/config.json`), workspace, and local config scopes.
4. **Argument parsing** — Parse command(s), subcommand, global flags, job flags. Resolve aliases.
5. **Routing** — Structured commands dispatch directly. Job commands proceed to the orchestration pipeline.
6. **Extension discovery** — Scan workspace projects, node_modules, and explicit references for `putnami.extension.json`.
7. **Contract validation** — Validate task I/O wiring before planning.
8. **Project selection** — Resolve the smart bare default or explicit `--impacted`, `--all`, `--projects`, `.`, `--tag`, and `--exclude` filters.
9. **Version computation** — Compute version metadata from git state (SHA, branch, tag, dirty status) for job subprocesses.
10. **Planning** — Match commands to extensions, expand pipelines, resolve cross-project dependencies.
11. **Scheduling** — Execute the DAG with parallel goroutines, cache integration, hook execution.
12. **Session finalization** — Write audit trail, track telemetry, write profiler output.

### Command Types

The CLI distinguishes two categories:

| Type | Examples | Behavior |
|------|----------|----------|
| **Structured commands** | `extensions`, `projects`, `workspace`, `deps`, `cache`, `config`, `sessions`, `version`, `migrate`, `completion`, `telemetry` | Fixed subcommand tree. No workspace required for some (e.g., `workspace init`, `telemetry`). |
| **Job commands** | `build`, `test`, `lint`, `serve`, `format`, `publish` | Provided by extensions. Require a workspace. Support multi-command syntax (`lint,test,build`), project selection, caching, watch mode. |

## Comparison with the TypeScript CLI

| Aspect | Details |
|--------|---------|
| Startup time | ~5ms |
| Memory usage | ~15 MB |
| Distribution | Single static binary |
| Extension loading | Manifest-only (JSON) |
| Flag discovery | Reads manifest `flags` field |
| Parallelism | Goroutine worker pool |
| File watching | Polling-based with Go timers |
| Output rendering | Text, live TUI (spinners/progress), JSONL, cloud-logging |
| Profiling | Chrome trace format (`--trace-profile`) |
| Session recording | Full audit trail with events and plan snapshots |

## Module Structure

```
module go.putnami.dev/tooling/cli    # tooling/cli/     — the orchestrator
module go.putnami.dev/cli/model      # tooling/cli-model/ — the data model

go 1.25.7
```

No third-party dependencies in either module. `tooling/cli` requires only in-repo
modules — `go.putnami.dev/protocol/*` and `go.putnami.dev/cli/model` — all
`replace`d to local paths.

`tooling/cli-model` requires **only `go.putnami.dev/protocol/*`**, and that
restriction is the boundary itself: the model cannot import the CLI, so a symbol
split onto the wrong side does not compile. See
[ADR 0006](adr/0006-cli-model-and-command-verticals.md).

## Exit Codes

| Code | Constant | Meaning |
|------|----------|---------|
| 0 | `ExitSuccess` | Success |
| 1 | `ExitError` | Failure: a job/build/test failed, or an unexpected internal error |
| 2 | `ExitUsage` | Usage: bad flags/args, an unknown command, no projects/jobs matched (when `--impacted` is set, empty results exit 0 instead), or a contract/config validation error |
| 3 | `ExitAuth` | Auth: authentication or authorization failed |
| 4 | `ExitAPI` | API: a remote or upstream API call failed |
| 130 | `ExitSignalReceived` | SIGINT (Ctrl+C) received |

The taxonomy is owned by `go.putnami.dev/protocol/cli` — the single source of truth shared with the extension SDK and cloud's cli-core — so exit codes mean the same thing across every Putnami command.

## Design Records

Architecture decisions live under [`adr/`](adr/), one decision per file, never
rewritten in place — a reversal gets a new ADR that supersedes the old one.

| Record | Covers |
|--------|--------|
| [ADR 0001 — CLI foundation boundaries](adr/0001-cli-foundation-boundaries.md) | One command catalog, one canonical result model, one engine; the telemetry seam; the deliberate self-execs |
| [ADR 0002 — CLI vNext contracts](adr/0002-cli-vnext-contracts.md) | Extension contract 3 and the three wire versions that move with it; typed identity; task-owned cache entries; machine output v2; the ratchets that enforce all of it |
| [ADR 0004 — Agent-artifact ownership](adr/0004-agent-artifact-ownership.md) | Materializing agent content: ownership is proof, one collision aborts everything, no force overwrite, undecidable resolves to preserve |
| [ADR 0047 — Extension-owned agent content](adr/0047-extension-owned-agent-content.md) | Why `extension:<name>` is the only agent-content opt-in, why the extension's pin is the content's pin, the run-wide ordering, retirement, and the refusal of separately declared artifacts |
| [ADR 0006 — cli-model and command verticals](adr/0006-cli-model-and-command-verticals.md) | The two boundaries: a model module that cannot import the CLI, and one package per command surface with `shared`/`sharedtest`; the seven rules for a mechanical move; the two ratchets that hold both |
| [ADR 0010 — The compatibility budget](adr/0010-compatibility-budget.md) | A stated version window per artifact format, what migrates, what is rejected on purpose, and prior-release bytes as the evidence |
| [ADR 0011 — Release governance and the neutral contributor path](adr/0011-release-governance-and-neutral-ci.md) | The release owner as a role bound to the license; exceptions that expire in the register that owns the rule; the contributor path proved by executing its own documentation |
| [ADR 0012 — Install trust and the release smoke](adr/0012-install-trust-and-release-smoke.md) | Why the public installer refuses what it cannot verify, never escalates, and enforces the platform matrix; why the smoke's install leg is `install.sh` itself |
| [ADR 0028 — The `putnami` repository is the source of its own CLI](adr/0028-self-hosted-cli-source-workspace.md) | Why a published CLI pin is exact and fails closed; why a producer declares `cli.source: "workspace"` instead; why plain removal fails open; how `./putnamiw` keys its build on tree content |
| [ADR 0038 — Nearest ownership and ordering-only scope edges](adr/0038-nearest-ownership-and-ordering-only-scope-edges.md) | Two ownership relations (holds, reads) and two edge families (ordering, impact): why a nested path has one directory owner, why an activated scope's implicit edge never carries `--impacted`, and the scope-config gap accepted with its follow-up |
