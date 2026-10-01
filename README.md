<h1 align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="sites/putnami.dev/public/assets/putnami-logo-white.svg">
    <img src="sites/putnami.dev/public/assets/putnami-logo.svg" alt="Putnami" width="320">
  </picture>
</h1>

> **Build systems, not apps.**
> Putnami is designed for your teams and their coding agents to build and run a
> multi-service system in one repository, and to keep it working as both grow.
> It is polyglot by design: Go and TypeScript ship today, and every boundary
> between languages is a tested contract.

**Status:** 0.2.0, released 2026-10-01, is the first public release, after
years of private development and use. The repository history starts at that
release.
A failure in the [supported core](#supported-core) is a bug, and we fix it:
[report it](https://github.com/putnami/putnami/issues/new?template=bug_report.md).
This repository is free, and stays free. See [What is free](#what-is-free).

## Quick Start

On macOS or Linux, on `amd64` or `arm64`, from an empty directory:

```bash
curl -fsSL https://putnami.dev/install.sh | bash
export PATH="$HOME/.putnami/bin:$PATH"
putnami init --project webapp --extension ts
putnami serve webapp
```

On Windows 10 version 1803 or later, or Windows 11, on `amd64`, in PowerShell:

```powershell
irm https://putnami.dev/install.ps1 | iex
putnami init --project webapp --extension ts
putnami serve webapp
```

Open the local URL that `putnami serve` prints. You are running the full system
locally.

You do not install Bun: Putnami installs Bun under `~/.putnami` for the
TypeScript starter when the machine holds none that fits. Windows needs Git for
Windows, Win32 long paths enabled, and Docker Desktop for `putnami compose`;
`putnami doctor` checks the long-path settings. The installer verifies each
download against the digest the registry advertises, refuses what it cannot
verify, and never asks for `sudo` or administrator rights. `putnami upgrade
--global` updates the CLI later.

Every prerequisite, the `PATH` rules, and the workaround when Microsoft Defender
blocks the Windows one-liner are in
[Installing the CLI](tooling/cli/doc/22-installing-the-cli.md). The
[getting-started guide](https://putnami.dev/docs/getting-started) covers the
rest of the first run.

## What Putnami Is

Putnami is one toolchain and one set of application frameworks for a system
made of several services in several languages. One CLI builds, tests, lints,
and runs every project in the repository. It knows which projects a change
affects, checks only those, and reports every result as data that a person or a
coding agent can act on.

### What is different

- **Contracts between languages.** Every boundary in the system, such as HTTP
  routes, configuration, events, or migrations, is a small versioned protocol
  with a JSON schema, a set of fixtures, and conformance tests. The CLI, the Go
  framework, and the TypeScript framework are tested against the same fixtures,
  so the languages cannot drift apart silently. The protocols live in
  [`protocols/`](protocols).
- **Built for coding agents.** Every workspace ships an MCP server, which
  exposes the workspace to an agent as typed tools. An agent can list projects,
  compute what a change affects and why, find the owner of a file, run lint,
  test, and build, and read each failure as `file:line:col` with a severity and
  a code. One tool changes anything, it supports a dry run, and it refuses jobs
  that reach external systems. The lint refuses a skipped test that has no
  stated reason, so an agent cannot fix a test by turning it off. See
  [Agents](https://putnami.dev/docs/agents).
- **Polyglot by design.** A language is an extension, built with the
  [extension SDK](tooling/extension-sdk), and every boundary is a
  language-neutral protocol. Go and TypeScript are supported today, and Python
  is experimental. A new language plugs into the same contracts.

### Designed to scale with teams and agents

More contributors and more agents mean more changes at once. Putnami keeps each
one bounded: a change is checked against exactly the projects it affects, every
result is data rather than a log to interpret, and an agent acts through one
tool that declares what it touches. Decisions live in versioned files that bind
every contributor, human or agent, and `putnami validate` enforces the ones a
machine can check.

Putnami is built this way. This repository is one workspace of 155 projects in
three languages, changed by people and coding agents through the same gates.
The repository of Putnami Cloud builds, tests, and deploys through Putnami on
every merge to `main`.

### When to pick something else

- You only need build caching for an existing repository: use Nx, Turborepo,
  or moon. Putnami also provides application frameworks and conventions, which
  you do not need for that.
- You have an existing repository: today Putnami starts new workspaces.
  Migrating a repository, or running Putnami next to your current tooling, is
  the next path we explore.
  [Tell us your stack](https://github.com/putnami/putnami/issues/new?template=share-your-stack.md)
  so we build the path that helps you.
- You ship one service and never plan to delegate its operation: the structure
  costs more than it returns. Reach for something lighter.

[Why Putnami](https://putnami.dev/docs/why) states what Putnami is not, what its
constraints cost you, and where it is today.

### The stack

| Layer | What it does | Status today |
|---|---|---|
| **Workspace** | Groups projects and extensions across languages in one repository. | Supported |
| **Runtime** | Runs application code through a language extension, with one CLI workflow for every language. | Supported for Go and TypeScript; Python is experimental |
| **Framework** | Web, API, auth, persistence, events, storage, and validation. PostgreSQL is the supported database. | Supported for Go and TypeScript; no Python framework yet |
| **Platform** | Runs the system outside your machine. | Today: a container image that you deploy anywhere. Next: managed environments, branch previews, and `putnami deploy`; see [Product Direction](#product-direction) |

## Principles

These are constraints, not aspirations. A violation is a bug.

**Observable by construction.** Every runtime emits structured,
machine-readable signals by default. Local, preview, and production expose the
same shape.

**Performance is a constraint.** Defaults survive production load. CI detects
regressions, and abstractions expose their cost.

**Deterministic and reviewable.** Git defines system intent. A deployment is a
pure function of versioned state, and drift is detectable.

**Security is foundational.** Defaults are secure. The system fails closed, and
an unsafe path requires explicit acknowledgment.

**Data ownership is non-negotiable.** Data lives in stores you control. Schemas
are versioned, portable, and part of the system contract.

**Automation is a first-class user.** The CLI is the primary interface. Its
output is deterministic and machine-readable, so humans and machines follow the
same rules. Metrics, logs, traces, and deployment state are exposed in formats an
AI agent can read, so an agent can operate the system, not only write its code.

## Using the CLI

A new workspace looks like this:

```
./
├── putnami.workspace.json # Identifies the workspace root
├── webapp/                # A project — one putnami.json per project
│   ├── putnami.json       # Project identity, extensions, and options
│   └── src/
│       ├── main.ts        # Application entrypoint
│       └── app/           # File-based routing
├── AGENTS.md, CLAUDE.md   # Assistant guidance block
└── .putnami/              # Installed extensions, cache, and generated tooling
```

Projects can live anywhere under the workspace root; `putnami init
--project-path` chooses where. Grouping them under `apps/` or `packages/` is your
convention, not a Putnami requirement.

```bash
putnami serve my-app                  # Run one application locally
putnami lint,test,build --impacted    # Check the projects your change affects
putnami build --all                   # Build the whole workspace
putnami package --projects my-app     # Build the container image of a project that declares docker
putnami publish --projects my-app     # Push that image to the configured registry
```

Every command accepts `--output=json` for machine-readable output and `--quiet`
to silence progress. `putnami --help` lists the rest.

## Documentation

Full documentation is at **https://putnami.dev**.

| I want to… | Go to |
|---|---|
| Install the CLI and create a workspace | [Getting started](https://putnami.dev/docs/getting-started) |
| Understand the system model | [Concepts](https://putnami.dev/docs/concepts) · [Principles](https://putnami.dev/docs/principles) |
| Build something specific | [How-to guides](https://putnami.dev/docs/how-to) |
| Learn the CLI, workspace, jobs, and caching | [Tooling & workspace](https://putnami.dev/docs/tooling-&-workspace) |
| Find a package's documentation | [TypeScript](https://putnami.dev/docs/frameworks/typescript) · [Go](https://putnami.dev/docs/frameworks/go) · [Python (experimental)](https://putnami.dev/docs/frameworks/python) |
| Know what I can depend on | [Support status](https://putnami.dev/docs/support) |
| Write a feature, spec, and decision record | [Write a feature spec](https://putnami.dev/docs/how-to/write-a-feature-spec) |
| Understand CLI telemetry and turn it off | [CLI telemetry and your rights](https://putnami.dev/docs/concepts/cli-telemetry) |
| Deploy and operate | [Platform](https://putnami.dev/docs/platform) |

Every page offers **Copy as Markdown**, and `https://putnami.dev/llms.txt` is a
machine-readable index of the whole tree.

## First Public-Release Contract

This section states what the first public release promises. The
[`putnami.support.json`](putnami.support.json) catalog is the machine-readable
authority for each package's support status, and the
[support status page](https://putnami.dev/docs/support) is generated from it.

### Supported core

The stable core is the Putnami CLI (`@putnami/cli`), Go support
(`@putnami/go`), and TypeScript support (`@putnami/typescript`), on these
targets:

| Operating system | amd64 | arm64 |
|---|---|---|
| macOS (`darwin`) | Supported | Supported |
| Linux | Supported | Supported |
| Windows 10 version 1803 or later, Windows 11 | Supported | Not supported |

`install.sh` and `install.ps1` refuse any other platform before they download
anything, and name what they detected. The release does not build
`windows/arm64` (decision D-W1 in [`tooling/cli/decisions.json`](tooling/cli/decisions.json)).

Windows support covers people who install the published CLI, extensions, and
templates. Contributors to this repository work on macOS or Linux, because
`putnamiw` and the contributor checks do not run on Windows.

Each release runs the Quick Start commands on all five supported targets, with
an empty home, config, and cache and no credentials, and sends a real HTTP
request to the generated starter. On Linux, each release also pastes the Quick
Start block into an image that holds Bash, `curl`, `tar` and a SHA-256 tool and
nothing else, then runs `putnami lint,test,build` and the Go starter there.
Those checks do not cover other shells, offline installs, production
deployment, or long-term source compatibility of the generated app. Digest verification proves the bytes match what the registry
advertises; it is not artifact signing. The details are in
[The release smoke](tooling/cli/doc/22-installing-the-cli.md#the-release-smoke).

### Not covered

- Python (`@putnami/python`) is experimental. It is the extension and its
  templates only: no Python framework package ships, it is never enabled by
  default, and it carries no parity promise with Go or TypeScript.
- Private services, internal tooling, and internal infrastructure are outside
  this promise.
- Code present in this repository is not stable until the support catalog says
  so. Support status is also not feature maturity: maturity records what this
  repository has proven, and is defined in
  [`protocols/features`](protocols/features/README.md).

### Versions and compatibility

Before v1.0.0, the stable core may change in breaking ways. Each breaking release
lists the changes you must make in its release notes or migration documentation,
and a minor release does not promise compatibility with earlier minor releases.

[Compatibility and Migration](tooling/cli/doc/21-compatibility-and-migration.md)
states, per artifact format, which versions it reads, what migrates, what is
rejected on purpose, and the command to run when your file is outside the
window. [ADR 0010](tooling/cli/doc/adr/0010-compatibility-budget.md) records
the decision. A test pins every number in that document against artifacts of
earlier releases.

### Licensing

The first public release is licensed under [FSL-1.1-MIT](LICENSE.md), not MIT.
In plain terms: you can use Putnami for any purpose, including commercial
production and services you deliver to clients, except offering a commercial
product that competes with Putnami. Each release becomes MIT-licensed two years
after its publication, and nobody can revoke that. The license file and its
future-license terms control; this summary does not replace them. We intend to
publish v1.0.0 under the MIT License; that intent does not change the license
that applies today.

### What is free

This repository is free and stays free: the CLI, the language extensions, the
frameworks, the protocols, and the templates. With it alone, you build, test,
and package a system, and you work on it with coding agents. The container
image it produces runs anywhere you deploy containers.

Putnami Cloud will offer hosted services on top of it. The offers are not
published yet; [putnami.com](https://putnami.com) will describe them. The
details are in [What is free](https://putnami.dev/docs/concepts/what-is-free).

### Who decides

[GOVERNANCE.md](GOVERNANCE.md) records who approves a release, a support-status
change, or an exception, and how a rule is waived. [RELEASING.md](RELEASING.md)
covers the release cadence, checklist, patch policy, and rollback.
[RELEASE.md](RELEASE.md) holds the release plan.

## Product Direction

The following experience is where Putnami is heading. It is not part of the
first public-release promise above.

- **Local development**: `putnami serve` runs one service or a hundred, starts
  every dependency, reloads the whole system on change, and unifies structured
  logs.
- **Branch previews**: every pushed branch gets its own environment, with the
  full system, isolated data, and automatic cleanup.
- **Production deploy**: `putnami deploy` ships deterministic builds with
  zero-downtime rollout, fast rollback, and the same system you tested locally.
- **More languages**: mobile (iOS and Android) and Rust, as language
  extensions that implement the same protocols.
- **Existing repositories**: migrating a repository to Putnami, and running
  Putnami next to existing tooling.

## Telemetry

The CLI records a small, closed set of anonymous usage signals and sends them to
`telemetry.putnami.dev`, whose receiver lives in this repository at
[`sites/telemetry.putnami.dev`](sites/telemetry.putnami.dev). Collection starts
only after an interactive notice and is off in CI by default.
`putnami telemetry off`, `DO_NOT_TRACK=1`, or `PUTNAMI_TELEMETRY=off` turn it
off. What is collected, stored, and for how long is in
[CLI telemetry and your rights](https://putnami.dev/docs/concepts/cli-telemetry).

## Repository Structure

The repository is organized by ownership; each language ecosystem is
self-contained.

| Directory | What it owns |
|---|---|
| `tooling/` | CLI, extension SDK, scaffold, client generator |
| `protocols/` | Wire contracts (schemas and fixtures) shared by frameworks, tooling, and platform |
| `typescript/` | TypeScript extension, frameworks, templates, and samples |
| `go/` | Go extension, frameworks, templates, and samples (`go.putnami.dev/*`) |
| `python/` | Experimental Python extension, templates, and samples |
| `sites/` | [`putnami.dev`](sites/putnami.dev) (documentation) and [`telemetry.putnami.dev`](sites/telemetry.putnami.dev) (CLI usage receiver) |

Each language directory has the same shape: `extension/`, `framework/`,
`templates/`, `samples/`. The full layout rules are in
[CONTRIBUTING.md](CONTRIBUTING.md#repository-layout).

Samples:

- [`typescript/samples/`](typescript/samples): a progressive learning path, from `01-hello-world` to `14-capabilities`
- [`go/samples/`](go/samples): library, application, task API, and protocol examples
- [`python/samples/`](python/samples): experimental library and application examples
- [`tooling/samples/`](tooling/samples): extension development examples

## Contributing

Contributions are welcome when they reinforce the system's constraints and
guarantees.

```bash
git clone https://github.com/putnami/putnami.git
cd putnami
./putnamiw install
```

Use `./putnamiw`, the committed wrapper, for every command. This repository
produces the CLI, so the wrapper builds it from the tree, and a globally
installed `putnami` is refused here. Building, testing, and linting need no
account and no credentials; see
[Contributor CI Without Putnami Cloud Credentials](CONTRIBUTING.md#contributor-ci-without-putnami-cloud-credentials).

Read [CONTRIBUTING.md](CONTRIBUTING.md) and
[CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md) first. Report a vulnerability through
[SECURITY.md](SECURITY.md), never in a public issue.

A change that adds or alters user-visible behavior carries a feature
declaration, a spec, and, for a durable choice, a decision record.
[Write a feature spec](https://putnami.dev/docs/how-to/write-a-feature-spec)
walks through it, and [`protocols/features`](protocols/features/README.md)
defines the contracts behind it.

## License

[LICENSE.md](LICENSE.md) is the license in force: today, **FSL-1.1-MIT**. See
[Licensing](#licensing) in the release contract for the intended change at v1.0.0.
