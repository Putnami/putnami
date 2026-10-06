# Getting Started

## Installation

### From Release

Download the latest binary for your platform:

```bash
curl -fsSL https://putnami.dev/install.sh | bash
```

To install a specific version:

```bash
curl -fsSL https://putnami.dev/install.sh | bash -s -- --variant go --version 1.2.0
```

The binary is installed to `~/.putnami/bin/putnami`. The installer makes `putnami`
reachable itself — it links into a writable directory already on your `PATH`, or
prints the exact line to run in the current shell and appends it to your shell
startup file:

```bash
export PATH="$HOME/.putnami/bin:$PATH"
```

It verifies the download against the digest the registry advertises, refuses what
it cannot verify, and never uses `sudo`.
[Installing the CLI](22-installing-the-cli.md) is the full contract: every flag,
every refusal, and the `PATH` behavior.

### Using `putnamiw` (Workspace Wrapper)

The repository includes a `putnamiw` wrapper script (similar to Gradle's `gradlew`). It bootstraps the CLI automatically — no manual install needed:

```bash
# Build from Go source (auto-downloads Go if not installed)
./putnamiw build --impacted

# Force rebuild from source
./putnamiw --bootstrap build --impacted

# Download a pre-built binary instead of compiling (no Go required)
./putnamiw --download build --impacted
```

| Flag            | Purpose                                                                                 |
|-----------------|-----------------------------------------------------------------------------------------|
| (none)          | Build from Go source if binary is missing or stale. Reuses cached binary otherwise.     |
| `--bootstrap`   | Force rebuild from source, even if the binary is up to date.                            |
| `--download`    | Download a pre-built binary from `putnami.dev` instead of compiling. No Go required.    |
| `--print-engine` | Build or reuse the from-source CLI, run nothing, and print where it is. Workspaces with `tooling/cli` only. |

The binary is cached at `.putnami/bin/putnami`. On subsequent runs, `putnamiw` skips the build/download if the binary is already present and up to date.

#### Build the engine without running it

A launcher that starts the CLI itself, for example a hosted CI launcher that hands credentials to the CLI process only, builds the engine first:

```bash
./putnamiw --print-engine
```

It prints one JSON line on stdout and nothing else:

```json
{"path":"/home/ci/.putnami/artifacts/cli-source/<key>/putnami","key":"<key>","root":"/work/repo"}
```

| Field  | Meaning                                                                        |
|--------|--------------------------------------------------------------------------------|
| `path` | Absolute path of the built binary: `$PUTNAMI_HOME/artifacts/cli-source/<key>/putnami`. |
| `key`  | The content key of the sources it was built from.                              |
| `root` | Absolute path of the workspace root.                                           |

The mode uses the same key, cache, and build as a normal run. It reuses the cached binary when the sources have not changed, and `--bootstrap` forces a rebuild. It starts nothing and writes nothing under `.putnami/bin`. Progress and errors go to stderr. On any failure it exits non-zero and prints nothing on stdout. It refuses a workspace without `tooling/cli`, a checkout with no content key (no git work tree, or no `sha256sum` or `shasum`), a command after the flag, and `--download`.

To start the printed binary as the workspace's own engine in a workspace whose lock records `cli.source: "workspace"`, run `path` itself, with its working directory inside `root`, and set:

| Variable                  | Value                                                         |
|---------------------------|---------------------------------------------------------------|
| `PUTNAMI_FROM_SOURCE`     | `root`                                                        |
| `PUTNAMI_FROM_SOURCE_KEY` | `key`                                                         |
| `PUTNAMI_HOME`            | The `PUTNAMI_HOME` the wrapper used, so that `path` is under it. Without it, the CLI looks in `~/.putnami`. |

The CLI admits itself only when it is the file at `$PUTNAMI_HOME/artifacts/cli-source/$PUTNAMI_FROM_SOURCE_KEY/putnami`, so start `path`, not a copy of it. `PUTNAMI_NO_RELAUNCH` has no effect in such a workspace. In a workspace whose lock pins a published CLI version, also set `PUTNAMI_NO_RELAUNCH=1`, or the binary relaunches into the pinned release.

**CI usage**: In consumer repositories, use `--download` in jobs that don't need Go (e.g., TypeScript, Python) to avoid installing the Go toolchain:

```yaml
steps:
  - uses: actions/checkout@v4
  - uses: oven-sh/setup-bun@v2
  - run: bun install --frozen-lockfile
  - run: ./putnamiw --download lint,test,build --all
```

Repositories that develop the Putnami CLI or change workspace planning should use the local wrapper path without `--download`, or build the current commit's CLI once and reuse that artifact across jobs. This keeps CI validating the same CLI and workspace config that are under review.

### From Source

Build from the repository root:

```bash
cd tooling/cli
go build -o putnami ./cmd/putnami
```

Or use the workspace build system:

```bash
putnami build tooling/cli
```

This builds the binary and installs it to `.putnami/bin/putnami-go-dev`. A `putnami` symlink is created automatically if none exists.

### Upgrading

To upgrade the CLI, extensions, and dependencies:

```bash
putnami upgrade
```

To upgrade only the CLI binary:

```bash
putnami upgrade --cli
```

See [version management](14-version-management.md) for the binary layout and naming conventions.

## First Commands

### Initialize a Workspace

```bash
mkdir my-workspace && cd my-workspace
putnami workspace init
```

This creates a `putnami.workspace.json` at the workspace root.

The local developer loop does not require `@putnami/cloud`. A workspace with
only the Go and/or TypeScript extensions can install dependencies, build, test,
serve, use the local action cache, and use the local MCP tools without cloud
credentials or network access. Cloud is an explicit capability boundary:
configured remote cache falls back to local-only with a one-line notice when no
cache provider is installed, while publish and deploy flows that require a
missing extension fail with a `putnami install` hint. Runtime remote config and
secrets sources are likewise registered only when the optional cloud runtime is
installed; local files and environment configuration continue to boot alone.

By default, scaffolded projects are registered under `packages/<project-name>`. That is a workspace convention, not a requirement. The Putnami monorepo itself uses a different ownership-based layout documented in [`CONTRIBUTING.md`](../../../CONTRIBUTING.md#repository-layout).

To initialize a workspace and scaffold the first project in a custom location:

```bash
putnami workspace init --project my-app --project-path apps/my-app
```

### Open the Workspace in Claude Code or Codex

The public installer registers Putnami with installed Claude Code and Codex
hosts. Open an existing Putnami project in either host and start working; the
stable launcher resolves the active session's worktree and its pinned Putnami
CLI. No workspace identifier or separate Intelligence setting is required.
Host trust and managed policy can still require their normal approval.
If a resumed or delegated agent inherits an MCP server bound to another
worktree, the live bootstrap detects the root mismatch and uses Putnami's local
CLI in the active worktree for that question; reconnecting MCP is optional.

For read-only analysis, Putnami prepares only registry extensions pinned by
`putnami.lock.json`, under a short deadline. It does not run `putnami install`,
dependency installation, Cloud setup, workspace jobs, or context generation.
The MCP server then advertises the exact `AI.md` from each prepared extension
and any extension-provided version-resolved documentation tools. If the exact
artifact is unavailable offline, the read continues immediately with local
workspace tools and reports the unavailable locked version; it never substitutes
the latest release. Existing host configuration, human `.agents/constraints.md`,
generated-file ownership markers, and agent-artifact receipts are preserved.

See [Agent Workflows](18-agent-workflows.md) for the guidance and ownership
model, and [Installing the CLI](22-installing-the-cli.md#claude-code-and-codex-registration)
for host registration, session behavior, and qualified host versions.

### Explore the Workspace

```bash
# List all projects
putnami projects list

# Describe a specific project
putnami projects describe my-app

# Show workspace configuration
putnami workspace describe

# List discovered extensions
putnami extensions list
```

### Run Jobs

```bash
# Build with smart default selection
putnami build

# Test only impacted projects (changes since main)
putnami test --impacted

# Lint and test the current project
putnami lint,test .

# Serve an application with hot-reload
putnami serve my-app --watch
```

### Use Aliases

Single-letter aliases speed up common commands:

| Alias | Command |
|-------|---------|
| `b` | `build` |
| `t` | `test` |
| `l` | `lint` |
| `s` | `serve` |
| `f` | `format` |
| `p` | `publish` |
| `P` | `publish` |
| `d` | `deploy` |
| `D` | `deploy` |

```bash
# These are equivalent
putnami build --impacted
putnami b --impacted
```

### Multi-Command Execution

Run multiple jobs sequentially in a single invocation:

```bash
# Lint, then test, then build — all with the same project selection
putnami lint,test,build --impacted
```

All commands share the same flags. If any command fails, subsequent commands are skipped (unless `--continue-on-error`).

### Preview Execution

Before running jobs, preview the plan:

```bash
putnami build --impacted --plan
```

This shows which projects and jobs would execute, their dependency order, and cache status — without running anything.

## Shell Completion

The install script sets up shell completions automatically. To install or update them manually:

```bash
# Bash
putnami completion bash > ~/.local/share/bash-completion/completions/putnami

# Zsh (oh-my-zsh)
mkdir -p ${ZSH_CUSTOM:-~/.oh-my-zsh/custom}/completions
putnami completion zsh > ${ZSH_CUSTOM:-~/.oh-my-zsh/custom}/completions/_putnami

# Zsh (without oh-my-zsh)
mkdir -p ~/.zfunc
putnami completion zsh > ~/.zfunc/_putnami
# Add to ~/.zshrc (before any compinit call):
#   fpath=(~/.zfunc $fpath)
#   autoload -Uz compinit && compinit

# Fish
putnami completion fish > ~/.config/fish/completions/putnami.fish
```

Restart your shell after installing (`exec zsh`, `exec bash`, or reopen the terminal).
`putnami upgrade --cli` refreshes the installed completion file for your current
shell. For zsh, clear the completion cache after an upgrade if completions still
look stale:

```bash
rm -f ~/.zcompdump*
exec zsh
```

Completions cover commands, subcommands, project names, flags, and tag values.

## Quick Reference

```bash
# Project selection
putnami build                      # Smart default (impacted on feature branches)
putnami build .                    # Current directory scope
putnami build --all                # Every project
putnami build --impacted           # Only changed projects (git diff vs resolved baseline)
putnami build --projects a,b,c     # Specific projects
putnami build --tag frontend       # Projects with a tag
putnami build --exclude-tag slow   # Exclude projects by tag
# Execution control
putnami test --no-cache            # Skip cache, force re-execution
putnami build --no-cache-projects a # Skip cache for `a` only; its dependencies keep it
putnami test --watch               # Re-run on file changes
putnami build --max-parallel auto  # Tune workers from hardware and job mix
putnami build --retry 2            # Retry failed jobs up to 2 times
putnami test --retry-failed        # Re-run tasks whose identical failure is cached
putnami build --continue-on-error  # Don't stop on first failure
putnami test --verbose             # Show job results
putnami test --debug               # Show all events in real-time

# Output formats
putnami build --output=jsonl       # Streaming JSONL events
putnami build --output=cloud-logging  # Google Cloud structured JSON

# Profiling
putnami build --all --trace-profile trace.json  # Generate Chrome trace
```
