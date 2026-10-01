# Getting Started

Putnami starts with one workspace model, then lets you choose the runtime surface that fits the first thing you want to build.

Use this page to get the CLI and workspace in place. If you already know the language you want, jump straight to the framework start guide:

| You want to build... | Start here |
|----------------------|------------|
| React SSR, API routes, or a TypeScript library | [TypeScript getting started](/docs/frameworks/typescript/getting-started) |
| A Go service or Go library | [Go getting started](/docs/frameworks/go/getting-started) |
| An experimental Python service, library, or data workload | [Python getting started](/docs/frameworks/python/getting-started) |

## Run the verified first-use path

From an empty directory, run these three commands in the same shell:

```bash
curl -fsSL https://putnami.dev/install.sh | bash
putnami init --project webapp --extension ts
putnami serve webapp
```

Supported platforms are macOS and Linux, on `amd64` or `arm64`, and Windows on
`amd64`, which has its own installer, below. Anything else stops with an error
naming what it detected, rather than installing a binary that cannot run.

Prerequisites are Bash, `curl`, `tar`, either `sha256sum` or `shasum`, Bun v1.4.0
or later for the TypeScript init/serve path, network access to `putnami.dev` and
the public artifact registry, network access to the public package registry
(`https://npm.putnami.dev`) used by `bun install`, and a standard user-writable
directory already named by `PATH`
(`~/.local/bin`, `~/bin`, the install directory, or a writable
`/usr/local/bin`). The home-directory candidate may be absent: the installer
creates it. That `PATH` prerequisite is what makes the second command
discoverable immediately; a script running as the child of a pipe cannot mutate
its parent shell.

**What the installer does, and does not do**

- **It verifies what it downloads.** The registry states the SHA-256 of the
  binary it serves; the installer computes that hash and compares. It prints
  `Integrity verified (sha256:…)` only after the comparison passes, and it
  refuses to install anything it cannot check.
- **It never asks for your password.** No `sudo`, ever. It installs into
  `~/.putnami/bin`, and links `putnami` only into a directory that is already on
  your `PATH` and writable by you. If there is no writable target, it says which
  directory and stops — pass `--install-dir <dir>` to choose another one.
- **It tells you if you need one more line.** When it cannot reach your `PATH`
  on its own, it prints the exact command to run in the shell you are in:

  ```bash
  export PATH="$HOME/.putnami/bin:$PATH"
  ```

  and appends it to your shell startup file for future shells. The last line it
  prints is which `putnami` your shell now runs, so you never have to guess.

Install a specific version with
`curl -fsSL https://putnami.dev/install.sh | bash -s -- --version 1.2.3`.

Already installed? Update any time with `putnami upgrade --global` — the
install script is only needed once per machine.

### On Windows

On Windows 10 version 1803 or later, or Windows 11, on `amd64`, open a
PowerShell window and type the same path:

```powershell
irm https://putnami.dev/install.ps1 | iex
putnami init --project webapp --extension ts
putnami serve webapp
```

Microsoft Defender blocks the first line when it is passed to `powershell -c`
on a command line. From `cmd.exe` or Git Bash, download the installer and run
it as a file:

```
curl.exe -fsSLo install.ps1 https://putnami.dev/install.ps1
powershell -NoProfile -ExecutionPolicy Bypass -File install.ps1
```

The PowerShell installer verifies the download the same way, never asks for
administrator rights, installs into `%USERPROFILE%\.putnami\bin`, and adds that
directory to your user `PATH`. Terminals that were already open keep their old
`PATH`; the installer prints the line to run in them.

Windows on `amd64` is supported for people who install and use Putnami. The
release gate runs these PowerShell commands on a Windows host created for each
QA run. Windows on `arm64` is not supported, and contributors to Putnami itself
work on macOS or Linux.

Windows needs a few more things:

- **Git for Windows.** Workspace hooks run with `sh`, which Git for Windows
  provides.
- **Long paths.** Enable `LongPathsEnabled` in Windows and set
  `git config --global core.longpaths true`. `putnami doctor` checks both and
  prints the command for each.
- **Bun v1.4.0 or later**, on `PATH` or where Bun's own installer puts it.
- **Docker Desktop**, only for `putnami compose`.

Prefer not to run an install script?
Read the [CLI reference](/docs/tooling-&-workspace/cli) for the command surface and install notes.

## What the gate proves

The release gate replays the exact macOS and Linux commands above with an
empty workspace, neutral home/config/cache directories, no credentials, and no
preinstalled CLI or extensions. `init` creates and validates:

- `putnami.workspace.json` identifies the workspace root
- `putnami.lock.json` pins the TypeScript extension and web template
- `webapp/putnami.json` identifies the generated project
- `package.json` and `bun.lock` record its dependency state
- `.npmrc` maps `@putnami` dependencies to the public package registry without credentials
- one Putnami block in `AGENTS.md` (which `CLAUDE.md` imports) gives coding assistants the local conventions

`serve` must emit machine-readable readiness, the starter must answer a real
HTTP request successfully, and shutdown must complete within the gate's bound.
Open the local URL printed by `putnami serve`.

This guarantee is deliberately narrow. It does not promise support for every
shell or Linux distribution, Windows on `arm64`, other architectures, offline
installation, proxy-specific configuration, production deployment, or stable
source details in every future generated app. Go and experimental Python
starters remain available through their framework guides, but they are not the
three-command golden path gated here. The checksum proves the bytes match the
public registry's digest; artifact signing is a separate, non-goal mechanism.

Experimental Python requires an explicit workspace opt-in after initialization:

```bash
putnami deps add @putnami/python
putnami extensions install
```

## What just happened

You interacted with three core pieces of Putnami:

- **Workspace** - the root container that organizes projects, extensions, templates, and shared context. [Learn more](/docs/concepts#workspace)
- **Project** - a runnable unit inside the workspace. [Learn more](/docs/concepts#project)
- **Template** - the scaffold that gives a new project its language-specific shape. [Reference](/docs/tooling-&-workspace/templates)
- **CLI** - the command surface for build, test, lint, serve, publish, dependencies, and templates. [Reference](/docs/tooling-&-workspace/cli)

You didn't configure anything yet. Defaults are intentional.

## Continue from here

Choose the path closest to your first project:

- [TypeScript getting started](/docs/frameworks/typescript/getting-started) - full-stack web apps, API services, and TypeScript packages
- [Go getting started](/docs/frameworks/go/getting-started) - compiled services, workers, and Go packages
- [Python getting started](/docs/frameworks/python/getting-started) - Experimental, explicit opt-in FastAPI services, libraries, and data workloads
- [Develop with AI assistants](/docs/how-to/develop-with-ai) - keep the same structure readable by humans and agents
- [Support status](/docs/support) - what each package and protocol commits to before you depend on it
