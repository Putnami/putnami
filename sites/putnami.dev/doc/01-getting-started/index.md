# Getting Started

Putnami starts with one workspace model, then lets you choose the runtime surface that fits the first thing you want to build.

Use this page to get the CLI and workspace in place. If you already know the language you want, jump straight to the framework start guide. If your team wants Putnami on a repository it already has, start from [an existing repository](/docs/getting-started/existing-repository) instead: it goes from a first `putnami observe` to a note on each pull request, without turning the repository into a workspace.

| You want to build... | Start here |
|----------------------|------------|
| React SSR, API routes, or a TypeScript library | [TypeScript getting started](/docs/frameworks/typescript/getting-started) |
| A Go service or Go library | [Go getting started](/docs/frameworks/go/getting-started) |
| An experimental Python service, library, or data workload | [Python getting started](/docs/frameworks/python/getting-started) |
| Putnami on a repository your team already has, from its CI to its pull requests | [Start from an existing repository](/docs/getting-started/existing-repository) |
| Putnami on a Rust, Java or Kotlin, Swift, Dart, .NET, Ruby or PHP repository you already have | Its reference, from [Languages and monorepo tools](/docs/languages) |
| Putnami on a repository Bazel, moon, Nx, Pants, Rush or Turborepo already builds | [Adopt Putnami on an existing monorepo](/docs/how-to/adopt-putnami-on-an-existing-monorepo) |

## Run the verified first-use path

From an empty directory, paste this block into your shell:

```bash
curl -fsSL https://putnami.dev/install.sh | bash
export PATH="$HOME/.putnami/bin:$PATH"
putnami init --project webapp --extension ts --template typescript-web
putnami serve webapp
```

The second line makes `putnami` reachable in the shell you are in. It changes
nothing when the command is already reachable.

Supported platforms are macOS and Linux, on `amd64` or `arm64`. Windows is not
supported for now: see [On Windows](#on-windows). Anything else stops with an error
naming what it detected, rather than installing a binary that cannot run.

Prerequisites are Bash, `curl`, `tar`, either `sha256sum` or `shasum`, network
access to `putnami.dev` and the public artifact registry, and network access to
the public package registry (`https://npm.putnami.dev`) used by `bun install`.
The first run also downloads the toolchains it needs: Bun from `github.com`,
with its release read from `api.github.com`, for a Go project, Go from
`go.dev`, and, for a workspace that adds `@putnami/python`, uv from
`github.com`.
No directory has to be on `PATH` beforehand: a script running as the child of a
pipe cannot change its parent shell, so the block sets `PATH` itself.

Bun is not a prerequisite. `putnami init` uses a Bun the machine already holds
when it fits. Otherwise Putnami installs Bun, at the release the lock pins,
under `~/.putnami/toolchains/bun/bun-<version>/`, from `github.com`, and
refuses an archive whose SHA-256 is not the one the lock records. It writes
nothing to `~/.bun` or to a directory on `PATH`.

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

  It appends that line to your shell startup file for future shells, and
  repeats it as the first command under `Next:`. It also says which `putnami`
  your shell now runs, so you never have to guess.

Install a specific version with
`curl -fsSL https://putnami.dev/install.sh | bash -s -- --version 1.2.3`.

Already installed? Update any time with `putnami upgrade --global` — the
install script is only needed once per machine.

### On Windows

Putnami does not support Windows for now. Windows support is suspended since
2026-11-26: no Windows build is published, and
`https://putnami.dev/install.ps1` installs nothing and says so. Support was last
verified on 2026-09-28, on a Windows Server 2022 test host; it comes back when
users ask for it. Use macOS or Linux, on `amd64` or `arm64`.

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

On Linux, the gate also pastes the block above, as printed, into an image that
holds Bash, `curl`, `tar` and a SHA-256 tool and nothing else. There it requires
HTTP 200 from every page of the starter and exit status 0 from
`putnami lint,test,build webapp`. It then requires the same from a Go starter:
`putnami init --project api --extension go`, `putnami serve api` and
`putnami lint,test,build api`.

This guarantee is deliberately narrow. It does not promise support for every
shell or Linux distribution, Windows, other architectures, offline
installation, proxy-specific configuration, production deployment, or stable
source details in every future generated app. The Go starter is gated on Linux
only, and the experimental Python starter is not gated. The checksum proves the bytes match the
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
- [Start from an existing repository](/docs/getting-started/existing-repository) - observe each change in your CI, then bring your specs and the verdict to the repository you already have
- [Adopt Putnami on an existing monorepo](/docs/how-to/adopt-putnami-on-an-existing-monorepo) - keep the build tool a repository already uses, and put Putnami's runner and verdict on it
- [Languages and monorepo tools](/docs/languages) - every language and tool Putnami runs, each with its reference
- [Develop with AI assistants](/docs/how-to/develop-with-ai) - keep the same structure readable by humans and agents
- [Support status](/docs/support) - what each package and protocol commits to before you depend on it
