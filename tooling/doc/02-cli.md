# CLI

The Putnami CLI is the single entry point for your workspace. It abstracts the underlying toolchains and provides a consistent interface for developers, CI systems, and AI agents.

New to a workspace? Follow the
[core workspace experience](../cli/doc/19-core-workspace-experience.md) from
workspace discovery through project and feature context, plan preview,
verification, MCP routing, and diagnostic recall.

## Installation

```bash
curl -fsSL https://putnami.dev/install.sh | bash
```

Supported platforms are macOS and Linux on `amd64` or `arm64`. On Windows 10
version 1803 or later, or Windows 11, on `amd64`, type this in a PowerShell
window:

```powershell
irm https://putnami.dev/install.ps1 | iex
```

From `cmd.exe`, download the installer and run it as a file; see
[Install on Windows](../cli/doc/22-installing-the-cli.md#install-on-windows).

It installs into `%USERPROFILE%\.putnami\bin` and adds that directory to your
user `PATH`. Windows also needs Git for Windows for `sh`, and Win32 long paths
enabled; `putnami doctor` checks the long-path settings.

Both installers compare the download against the SHA-256 the registry advertises
for it and refuse to install anything they cannot verify. The macOS and Linux
installer never uses `sudo`: it installs into `~/.putnami/bin` and links `putnami` only into a directory that is
already on your `PATH` and writable by you. If it cannot, it prints the exact
`export PATH=…` line to run and appends it to your shell startup file. See the
[Getting Started](/docs/getting-started) guide for the full walk-through.

The CLI is a single static Go binary with zero runtime dependencies. In a workspace with Go sources, you can also use the wrapper script `./putnamiw` which auto-builds the CLI from source.

## Job commands

Job commands are provided by extensions and run across projects:

```bash
putnami build .              # Build the current project
putnami test --impacted      # Test only what changed
putnami lint --all           # Lint the whole workspace
putnami serve my-app         # Serve an app with its dependencies
putnami package .            # Create distributable artifacts
putnami publish .            # Publish a package
putnami deploy my-app        # Deploy through a deployer extension
putnami format .             # Format code
```

Run multiple jobs in a single invocation:

```bash
putnami lint,test,build --impacted
```

Jobs run sequentially in the order specified, all sharing the same project selection.

### Command aliases

The verb vocabulary of `@putnami/dev`, which the language extensions install,
gives common jobs single-letter shortcuts:

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

Define custom aliases in `putnami.workspace.json`:

```json
{
  "aliases": {
    "ci": "lint,test,build",
    "dev": "serve"
  }
}
```

## Structured commands

These built-in commands manage workspace infrastructure:

### `putnami workspace`

- `putnami workspace init` — Initialize a new workspace in the current directory
  - `--project <name>` — Scaffold an initial project
  - `--project-path <path>` — Custom path for the initial project
  - `--channel <name>` — Resolve the extensions, the template and the starter's dependencies on one release channel (default: `PUTNAMI_CHANNEL`, then the channel the CLI was installed from, then `latest`). See [`workspace`](../cli/doc/03-commands.md#workspace)
- `putnami workspace describe` — Show workspace configuration
- `putnami init` — Alias for `workspace init`

### `putnami projects`

- `putnami projects list` — List all projects with paths and tags
- `putnami projects create <name> --template <template>` — Scaffold a new project from a template
- `putnami projects describe <project>` — Show project details
- `putnami projects sync` — Sync project configuration (e.g., Go workspace)
- `putnami projects tag <project> [tags]` — Manage project tags

### `putnami extensions`

- `putnami extensions install` — Install extensions from workspace config
- `putnami extensions install --platform <os>/<arch> --dest <dir>` — Materialize artifacts for another platform into a packageable artifact-store root (skips install hooks and the lock write)
- `putnami extensions update` — Update extensions to latest compatible versions
- `putnami extensions list` — List extensions with their version constraint, installed version and source
- `putnami extensions remove <ext>` — Remove an installed extension
- `putnami extensions install --user <@scope/name[@version]>` — Pin one extension in the user scope (`~/.putnami/user`), outside any workspace; its subcommands declared `workspace: "optional"` then run in any directory that is not a workspace
- `putnami extensions list --user` / `putnami extensions remove --user <ext>` — List or drop user-scope pins
- `putnami extensions validate [path]` — Validate an extension manifest
- `putnami extensions test [path]` — Run extension tests
- `putnami extensions package [path]` — Package an extension for distribution

### `putnami templates`

- `putnami templates install` — Install templates from `putnami.workspace.json`
- `putnami templates update` — Update to latest compatible versions
- `putnami templates list` — List configured and discovered templates
- `putnami templates remove <name>` — Remove an installed template
- `putnami templates validate [path]` — Validate a template manifest
- `putnami templates test [path]` — Test a template: render it, create a project from it, build and test it
- `putnami templates package [path]` — Package a template for distribution

### `putnami upgrade`

- `putnami upgrade` — Upgrade everything: CLI, extensions, templates, and framework dependencies
- `putnami upgrade --cli` — Only upgrade the CLI binary
- `putnami upgrade --global` — Upgrade the global CLI in `~/.putnami/bin` instead of the workspace pin; runs anywhere (no workspace required) and implies `--cli`
- `putnami upgrade --extensions` — Only upgrade extensions and templates
- `putnami upgrade --deps` — Only upgrade framework dependencies
- `putnami upgrade --channel canary` — Follow the canary channel on every registry (npm dist-tag, Go `@v/<channel>.info` per module, the put registry's channel projection)
- `putnami upgrade --release rs_<64-hex> --namespace putnami` — Use one exact immutable release set
- `putnami upgrade --version 1.2.3` — Upgrade to an exact Putnami release version
- `putnami upgrade --dry-run` — Resolve and print the plan without changing files

Flags can be combined. When no flags are given, all phases run against the `stable` channel. Every endpoint comes from the workspace `registries` entry for that ecosystem, and the archive downloads carry the user's credential. The resolution table shows the source revision of every version it resolved. There is no `--branch`: a branch is not a distribution channel. See the [Upgrade Putnami](/docs/how-to/upgrade-putnami) guide for details.

### `putnami deps`

- `putnami install` — Install workspace dependencies across all active technologies. Also accepts an extension name (e.g., `putnami install @putnami/go`) to download and install that extension.
- `putnami deps install` — Install dependencies and run extension workspace installers

### `putnami config`

- `putnami config show` — Show merged configuration
- `putnami config set <key> <value>` — Set a value (dot-notation paths supported)

### `putnami cache`

- `putnami cache clean [--all]` — Delete this repository's shared build cache, which every worktree of the repository reads; `--all` deletes every repository's store
- `putnami cache gc` — Garbage-collect the machine-global stores down to their byte budgets
- `putnami cache verify` — Run the workspace gate twice and fail on a cache key or restore that differs

`putnami cache` with no subcommand deletes nothing: it lists these subcommands and exits with a usage error.

### `putnami sessions`

- `putnami sessions list` — List recorded job sessions
- `putnami sessions inspect <id>` — Show details for a session

### `putnami cli`

- `putnami cli list [--global]` — List the installed CLI binaries in `.putnami/bin` (or `~/.putnami/bin` with `--global`) and mark the active one
- `putnami cli use <name> [--global]` — Switch the active CLI binary to an installed `putnami-<name>`

## Commands that left the core CLI

These commands left the core CLI. Most moved into the extension that owns
their domain, and work once a workspace declares that extension. `change-plan`
has no successor command: its document is the projection of an impact plan.
The `dev` root moved to the `extensions` and `templates` commands of the core,
which need no extension. The `contributor` root split: its machine load moved
to `putnami doctor --machine` in the core, and its language check moved to the
`change` command group of `@putnami/contributor`. The `tree` command keeps
`tree fingerprint`; `tree verify` moved to the same `change` group. The
`proposals` command group keeps the operations of the collaboration contract;
`proposals finalize` moved to the `change` group too. `version get` and
`version tag` keep their names and moved to `@putnami/dev`, which every
language extension depends on. `compose` and `qualify` keep their spellings
and moved to `@putnami/dev` too, as the `compose` and `qualify` command groups
whose default subcommand takes the project.

| Former command | Command now | Extension that serves it |
|---|---|---|
| `putnami channel set`, `putnami channel status` | `putnami cloud channels set`, `putnami cloud channels status` | `@putnami/cloud`, which owns hosted delivery |
| `putnami ci init`, `ci validate`, `ci fmt`, `ci explain` | `putnami cloud ci init`, `cloud ci validate`, `cloud ci fmt`, `cloud ci explain` | `@putnami/cloud` |
| `putnami features`, `specs`, `architecture`, `contracts` | Unchanged | `@putnami/sdd` |
| `putnami version list`, `putnami version use <name>` | `putnami cli list`, `putnami cli use <name>` | None: the core serves `cli` |
| `putnami version get`, `putnami version tag` | Unchanged | `@putnami/dev`, which every language extension depends on |
| `putnami compose <project>` | Unchanged, or `putnami compose up <project>` | `@putnami/dev` |
| `putnami qualify <project> --target local` | Unchanged, or `putnami qualify run <project> --target local` | `@putnami/dev` |
| `putnami infra plan [<project>]` | `putnami cloud infra plan [<project>]` | `@putnami/cloud` |
| `putnami change-plan --base <commit>` | `putnami impact-plan lint,test,build,validate --base <commit>`, then `ChangePlanFromImpactPlan` from `go.putnami.dev/protocol/ci` | None: the core serves `impact-plan` |
| `putnami dev extension validate [path]` | `putnami extensions validate [path]` | None: the core serves `extensions` |
| `putnami dev template validate [path]`, `dev template test [path]`, `dev template package [path]` | `putnami templates validate [path]`, `templates test [path]`, `templates package [path]` | None: the core serves `templates` |
| `putnami contributor language <mode>` | `putnami change language <mode>` | `@putnami/contributor` |
| `putnami contributor machine-load` | `putnami doctor --machine` | None: the core serves `doctor` |
| `putnami tree verify` | `putnami change verify` | `@putnami/contributor` |
| `putnami proposals finalize` | `putnami change finalize` | `@putnami/contributor` |

To get a moved command, add its extension to `extensions` in
`putnami.workspace.json`, then run `putnami install`:

```json
{
  "extensions": ["@putnami/cloud"]
}
```

The CLI refuses `putnami channel …`, `putnami ci …`, `putnami change-plan`,
`putnami infra …`, `putnami dev …`, `putnami contributor …`, `putnami
compose …` and `putnami qualify …`, and `putnami help` of each, with exit
code 2, unless an installed extension declares that root. It always refuses `putnami tree verify …`, `putnami proposals finalize …`
and `putnami help` of each, because the core still answers `tree` and
`proposals`. The refusal of `putnami dev …` names the `extensions` and
`templates` spellings, and the refusal of `putnami contributor …` names
`putnami doctor --machine`. The refusals of `putnami tree verify …` and
`putnami proposals finalize …` name no successor. The refusals link to this
section. A command that no loaded extension declares, a moved SDD command
included, is refused with exit code 2 and a message that says how to add an
extension.

## Project targeting

### Target expressions

```bash
putnami build /typescript/frameworks/web     # Exact project by ID
putnami build /typescript/...                # All projects under /typescript/
putnami build ./relative-path                # Relative to current directory
putnami build web                            # Alias (from projectAliases)
putnami build frontend                       # Group (from groups)
putnami build /a,/b,-/c                      # Union and subtraction
```

### Flags

| Flag | Purpose |
|------|---------|
| `.` | Current project (or all projects under current directory) |
| `--impacted` | Projects affected by git changes (propagates through dependency graph) |
| `--all` | Every project in the workspace |
| `--projects <list>` | Comma-separated project names |
| `--tag <tags>` | Filter by tags (comma-separated). Overrides workspace `excludeTags` for matched tags. |
| `--exclude-tag <tags>` | Exclude by tags (comma-separated) |
| `--exclude <names>` | Exclude by name (comma-separated) |
| `--baseline <branch>` | Custom git baseline for `--impacted`. Without it, `--impacted` resolves workspace `baseline`, upstream, `origin/HEAD`, then local `main`/`master`. A job command given `--baseline` with no target or `--all` implies `--impacted`. |

### Default behavior

When no target is given:

- **Feature branches** — targets projects impacted vs trunk (`origin/HEAD`, `origin/main`, then local `main`/`master`)
- **`main`/`master` with a local same-command/params marker** — targets projects impacted vs that marker SHA
- **`main`/`master` with no local marker and an active remote-cache run marker** — targets projects impacted vs the remote marker SHA, if that SHA resolves locally
- **`main`/`master` with no usable marker** — targets all projects

Use `.` when you want current-directory scope instead of the smart default.

## Execution flags

| Flag | Purpose |
|------|---------|
| `--no-cache` | Force re-execution (skip cache reads and writes; the stored entry stays) |
| `--watch`, `-w` | Re-run on file changes (150ms debounce) |
| `--plan` | Show execution plan without running |
| `--verbose`, `-v` | Show job results and diagnostics |
| `--debug` | Stream all job events in real-time (implies `--verbose`) |
| `--dry-run` | Show changes without applying |
| `--continue-on-error` | Don't abort on first failure |
| `--max-parallel <mode\|n>` | Parallelism policy (`auto`, `eco`, `max`) or explicit worker count |
| `--retry <n>` | Retry transient failures |

Job commands and extension command groups read `--watch`, `--plan`,
`--dry-run`, `--continue-on-error`, `--max-parallel` and `--retry`, among other
job-only flags. A core command that does not declare one refuses it with a
usage error that names the flag: `putnami projects list --dry-run` fails, while
`putnami projects sync --dry-run` runs.

## Output modes

| Flag | Format | Use case |
|------|--------|----------|
| *(default)* | Text with progress bars | Interactive terminal |
| `--output=jsonl` | Streaming JSONL events | CI integration, programmatic consumption |
| `--output=cloud-logging` | Google Cloud structured JSON | Cloud Run (auto-detected via `K_SERVICE`) |

Environment variables: `PUTNAMI_OUTPUT=jsonl`, `PUTNAMI_VERBOSE=true`, `PUTNAMI_QUIET=true`, `PUTNAMI_NO_COLOR=true`.

The `--output=jsonl` stream follows the session stream contract defined by the CLI protocol (`protocols/cli/schemas/result-v2.json`): `task:start` / `task:event` / `task:end` records per task, each stamped `protocolVersion: 2`, closed by one `session:end` aggregate. The subprocess runtime event a `task:event` carries is specified by `protocols/runtime/doc/04-output-contract.md`; the record around it by `protocols/cli/doc/02-result-v2.md`.

## Global flags

| Flag | Purpose |
|------|---------|
| `--help`, `-h` | Show help |
| `--version` | Show CLI version |
| `--quiet`, `-q` | Suppress non-error output |
| `--no-color` | Disable color output |
| `--color` | Force color output |
| `--profile <dev\|test\|production>` | Select the deployment profile (resolution: flag > `PUTNAMI_PROFILE` > workspace `profile` > `dev`) |
| `--trace-profile <path>` | Collect trace events (Chrome trace format) |

A flag goes after the full command: `putnami <command> [subcommand] [options]`.
A flag before the command word, as in `putnami -v projects list`, or between a
command and its subcommand, as in `putnami projects -v describe web`, is a
usage error that names the flag. `putnami --help`, `putnami --version`, and
flags alone, such as `putnami -v` or `putnami --output json`, still print help
or the version. Shell completion offers nothing after a flag that comes before
the command.

## Shell completion

The install script sets up completions automatically. Manual setup:

```bash
# Bash
putnami completion bash > ~/.local/share/bash-completion/completions/putnami

# Zsh (oh-my-zsh)
mkdir -p ${ZSH_CUSTOM:-~/.oh-my-zsh/custom}/completions
putnami completion zsh > ${ZSH_CUSTOM:-~/.oh-my-zsh/custom}/completions/_putnami

# Zsh (without oh-my-zsh)
mkdir -p ~/.zfunc
putnami completion zsh > ~/.zfunc/_putnami

# Fish
putnami completion fish > ~/.config/fish/completions/putnami.fish
```

`putnami upgrade --cli` refreshes the installed completion file for your current
shell. For zsh, clear the completion cache after an upgrade if completions still
look stale:

```bash
rm -f ~/.zcompdump*
exec zsh
```

Completions cover commands, subcommands, project names, flags, and flag values.

## Dynamic command loading

The CLI builds its command tree dynamically from installed extensions. This keeps the core small while ensuring the CLI always reflects what is actually installed:

- Different workspaces get different commands based on their extensions
- Help output stays accurate because commands are discovered at runtime
- AI agents see the same command surface as developers

Run `putnami --help` to see the full command tree for your workspace.
