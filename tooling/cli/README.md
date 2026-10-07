# go.putnami.dev/tooling/cli

Polyglot build orchestrator for monorepo workspaces. A single static Go binary that reads extension manifests, resolves a dependency DAG, and executes tasks in parallel via subprocesses.

## Features

- **Manifest-driven orchestration** — reads `putnami.extension.json` declaratively, never imports extension code
- **Multi-command execution** — `putnami lint,test,build --impacted` in a single invocation
- **Impact analysis** — `--impacted` runs only on projects affected by git changes, with transitive propagation
- **Parallel DAG scheduling** — goroutine worker pool with dependency-aware ordering
- **Content-addressed caching** — SHA-256 cache keys with upstream hash propagation
- **Watch mode** — polling-based file watcher with 150ms debounce, serve mode with process lifecycle management
- **Multiple output formats** — live TUI (spinners/progress), text, JSONL, Google Cloud Logging
- **Session recording** — full audit trail with execution plan snapshots and JSONL event streams
- **Shell completion** — bash, zsh, and fish
- **Zero dependencies** — stdlib-only Go, single static binary

## Layout

The CLI is two modules:

- **`tooling/cli`** (`go.putnami.dev/tooling/cli`) — the orchestrator. Its
  `internal/commands/` holds one package per command surface (twelve verticals:
  `agentctx`, `cachecmd`, `ci`, `completion`, `configcmd`, `doctor`,
  `extensions`, `lifecycle`, `migrate`, `sdd`, `sessions`, `versioncmd`), plus
  `shared` for production helpers two or more of them need and `sharedtest` for
  test-only ones. A vertical never imports a sibling without a written exception.
- **`tooling/cli-model`** (`go.putnami.dev/cli/model`) — the data model:
  `workspace`, `extension`, `jobs`. Type declarations and pure methods, nothing
  with an effect. **It requires only `go.putnami.dev/protocol/*` and may never
  import the CLI** — the dependency runs one way, and the module boundary is what
  enforces it.

[ADR 0006](doc/adr/0006-cli-model-and-command-verticals.md) records both
boundaries, the rules used to move the code, and the ratchets that hold them.

## Installation

```bash
curl -fsSL https://putnami.dev/install.sh | bash
```

macOS and Linux, on `amd64` or `arm64`. The installer verifies the download
against the digest the registry advertises for it, refuses what it cannot verify,
and never uses `sudo` — see
[Installing the CLI](doc/22-installing-the-cli.md) for the full contract.

Upgrade an existing install with `putnami upgrade --global` — the script is
only needed for the first install.

Or use the workspace wrapper (no manual install needed):

```bash
./putnamiw build --impacted
```

## Quick Start

```bash
# Initialize a workspace
putnami workspace init

# Install extensions, install hooks, agent workflows, and dependencies
putnami install

# Build with the smart default selection
putnami build

# Test only what changed
putnami test --impacted

# Lint, test, and build in one shot
putnami lint,test,build --impacted

# Serve with hot-reload
putnami serve my-app

# Preview execution plan
putnami build --impacted --plan
```

## Documentation

- **[Overview](doc/01-overview.md)** — Architecture, boot sequence, exit codes
- **[Getting Started](doc/02-getting-started.md)** — Installation, first commands, shell completion
- **[Commands](doc/03-commands.md)** — Full command reference (structured + job commands, flags, aliases)
- **[Job Execution](doc/04-job-execution.md)** — Planning, scheduling, subprocess protocol, caching
- **[Configuration](doc/05-configuration.md)** — Config schema, multi-scope merging, environment variables
- **[Workspace and Projects](doc/06-workspace-and-projects.md)** — Discovery, dependency graph, filtering, impact analysis
- **[Extensions](doc/07-extensions.md)** — Manifest schema, pipelines, contracts, hooks
- **[Output and Rendering](doc/08-output-and-rendering.md)** — Live TUI, text, JSONL, cloud logging renderers
- **[Watch Mode](doc/09-watch-mode.md)** — File watching, change classification, serve mode
- **[Caching](doc/10-caching.md)** — Cache key computation, including Go embedded asset inputs, stores, cache control
- **[Session Recording](doc/11-session-recording.md)** — Audit trail, event streams, session management
- **[Profiling and Telemetry](doc/12-profiling-and-telemetry.md)** — Chrome trace profiling, anonymous telemetry
- **[Internals](doc/13-internals.md)** — Go patterns, concurrency, error handling, design decisions
- **[Version Management](doc/14-version-management.md)** — Binary layout, upgrading, install script
- **[CI Change Plans](doc/17-ci-change-plans.md)** — Immutable impacted lint/test/build plans for CI admission
- **[Agent Workflows](doc/18-agent-workflows.md)** — Extension agent content materialized into `.agents`/`.claude`/`.codex`, the `init`/`install`/`upgrade` lifecycle, ownership and collision rules, and the migration from separately declared artifacts
- **[Core Workspace Experience](doc/19-core-workspace-experience.md)** — End-to-end discovery, selection, planning, execution, MCP, diagnostics, and support/maturity guidance
- **[Release Rehearsal](doc/20-release-rehearsal.md)** — The read-only rehearsal that records an explicit GO/NO-GO for the intended public release, its plan, and its rollback points
- **[Compatibility and Migration](doc/21-compatibility-and-migration.md)** — Supported version window per artifact format, what migrates, what is rejected, and the remedy
- **[Installing the CLI](doc/22-installing-the-cli.md)** — What the public installer trusts and refuses, the platform matrix, every flag, `PATH` behavior, and the release smoke
- **[Testing at Production Boundaries](doc/23-testing-at-production-boundaries.md)** — Recorded exchanges at the registry and credential boundaries, and how an incident closes with a test that fails on its pre-fix tree
- **[Workload Qualification](doc/24-workload-qualification.md)** — `compose` and `qualify`: serving a workload with what it runs with (stable proxy URLs, per-composition databases, typed readiness, orphan reaping), the smoke contract derived from the route inventory, the sha binding, and the fail-closed verdict
- **[Collaboration Providers](doc/25-collaboration-providers.md)** — Binding task, change-proposal and memory providers in `options.collaboration`, `putnami <contract> <operation>` and the matching MCP tools, capability discovery, and the outcomes that keep missing capabilities and uncertain writes explicit

## Command Reference

The command list is **generated from the command catalog**, never hand-written
([ADR 0001](doc/adr/0001-cli-foundation-boundaries.md) §1) — a table in a README
is one more copy that drifts:

```bash
putnami help                 # categorized command list with global flags
putnami help <command>       # one command: usage, flags, examples, related commands
putnami help --markdown      # the full reference as Markdown
putnami help --man           # the full reference as a man page
```

`putnami help --markdown` is the complete reference: job commands, every
structured command with its subcommands and flags, the global flag categories,
and the built-in aliases. See [Commands](doc/03-commands.md) for the concepts
around it (how job commands come from extensions, multi-command execution,
first-use auto-install, user-defined aliases).
