# Contributing to Putnami

Thank you for your interest in contributing to Putnami. This guide covers the process for contributing to this project.

## Getting Started

```bash
git clone https://github.com/putnami/putnami.git
cd putnami
./putnamiw install
```

`./putnamiw` is the committed wrapper, and it is what you run. A fresh clone has
no `putnami` on your `PATH`, and `install` does not put one there. **Where this
guide writes `putnami`, type `./putnamiw`.**

That is not a convention here, it is enforced. This repository *produces* the
CLI, so its `putnami.lock.json` records `"cli": { "source": "workspace" }` and
only a CLI built from this tree may run against it. A globally installed
`putnami` is refused with a message naming the `./putnamiw` command to run
instead, and `PUTNAMI_NO_RELAUNCH=1` does not bypass that. `./putnamiw` builds
the CLI from your checkout, caches it by tree content, and reuses it until you
change a source file — so the engine that gates a commit is always the engine
that commit contains. See
[ADR 0028](tooling/cli/doc/adr/0028-self-hosted-cli-source-workspace.md).

**Prerequisites**: `git` and network access. You do not need Go on your `PATH`:
`./putnamiw` resolves a toolchain for you, downloading a managed one from
`go.dev` when your system Go is older than the workspace pin. You do need a Go
*build* — `--download` is refused here, because a downloaded binary is not built
from your tree and the CLI would reject it. You do not need [Bun](https://bun.sh) either: TypeScript packages run with the
exact version declared in `package.json` (`packageManager: "bun@1.4.0"`) and
recorded with vendor archive digests in `putnami.lock.json`. `./putnamiw install`
uses a Bun of your machine when it reports that version, and installs that
version under `~/.putnami/toolchains/bun` otherwise.
Use the Putnami CLI for all workspace operations — never run npm, yarn, or
pnpm directly (`./putnamiw deps install` installs dependencies, `./putnamiw
upgrade --deps` upgrades them).

You need no Putnami account and no credentials to do any of this. See
[Contributor CI Without Putnami Cloud Credentials](#contributor-ci-without-putnami-cloud-credentials).

## Development Workflow

### 1. Create a Branch

```bash
git checkout -b feat/my-change
```

Use a descriptive branch name with a conventional prefix: `feat/`, `fix/`, `docs/`, `refactor/`, `test/`, `chore/`.

### 2. Make Changes

Use the `putnami` CLI for all workspace operations:

```bash
# Build impacted packages
putnami build --impacted

# Run tests
putnami test --impacted

# Lint
putnami lint --impacted

# Preview what will run
putnami build --impacted --plan
```

### 3. Validate Before Committing

This is **mandatory** before every commit:

```bash
putnami lint,test,build,validate --impacted
```

`validate` checks a project's features and specs, and plans `validate-workspace`
with them to check the workspace's declarations once. All four commands must
pass. The
gate is static evidence; for a change that reaches a workload, the
`putnami qualify <project> --target local` verdict is the execution evidence. If
something fails, use `--output=jsonl` for structured diagnostics:

```bash
putnami test --impacted --output=jsonl
```

Coverage is measured and enforced by default. Every `putnami test` instruments
and applies each project's `coverage-threshold`, so a drop fails the run that
caused it rather than surfacing later in CI. Two separate escape hatches:

| Flag | Effect |
|------|--------|
| `--no-enforce-coverage` | Still measures and reports; the threshold cannot fail the run |
| `--coverage=false` | Skips instrumentation entirely — the fast inner loop |

The per-project `coverage: false` policy is authoritative: a project that sets it
is neither measured nor gated, whatever the run passes. To run the full gate
automatically before every push, opt in to the committed pre-push hook:

```bash
git config core.hooksPath tooling/hooks
```

The hook is opt-in only — nothing activates it for you, and contributors who do
not enable it are unaffected.

### 4. Commit and Open a PR

Write clear commit messages. PR titles follow [Conventional Commits](https://www.conventionalcommits.org/):

```
type(scope): summary
```

Examples:
- `feat(application): add middleware chaining`
- `fix(sql): handle connection pool timeout`
- `docs(react): update SSR guide`

The PR title becomes the squash merge commit message, so keep it accurate as the PR evolves.

## Contributor CI Without Putnami Cloud Credentials

Everything you need to build, test, and lint Putnami is in this repository. You
do not need a Putnami account, a Putnami Cloud token, access to the private
artifact store, or the remote build cache. There is no contributor tier that
unlocks a working build.

From a fresh clone, this is the whole path:

<!-- neutral-contributor-ci:begin -->
```bash
./putnamiw install
./putnamiw lint,test,build,validate --impacted --enforce-coverage
```
<!-- neutral-contributor-ci:end -->

`./putnamiw` is the committed wrapper, and here it is the only entrypoint that
works. It builds the CLI from this workspace's source, resolving Go for you:
your system Go when that satisfies the workspace pin, and otherwise a managed
toolchain downloaded from `go.dev`. It caches that build by tree content, so
only the first run of a given tree state pays a compile. `--download` fetches a
pre-built binary in *consumer* workspaces; in this one it is refused, because a
downloaded binary is not built from your tree. `install` then installs the
extensions this workspace builds from its own source. Without credentials it
sends no request to the Putnami registry, so you need network access, not an
account.

Those two commands are executable evidence, not a description of one.
`tooling/cli-documents/neutral_contributor_ci_test.go` reads this exact block
out of this file and runs it against a workspace with:

- every Putnami Cloud, cache, registry, and cloud-provider credential removed
  from the environment;
- the registry and cache endpoints pointed at a server that refuses every
  request and counts the attempts;
- an empty artifact store, and a lock with no installed artifacts.

The run has to succeed, has to actually execute the impacted project's jobs, and
has to make zero requests to either endpoint. Editing the block above without
changing what really works fails the build.

### What is different without credentials

| Capability | Without Putnami Cloud credentials |
|---|---|
| `lint`, `test`, `build`, `serve` | Work, with identical results |
| Build cache | Local only. If a remote cache is configured with no provider, the CLI prints one notice and builds locally |
| Hosted Intelligence tools | Unavailable, which is the designed fail-closed answer. The local MCP workspace tools still work |
| `publish`, `deploy` | Fail with a message naming the extension that is missing. Maintainers run these; a contribution never needs them |

### Maintainer CI

Putnami Cloud CI runs on this repository through its native runner. The gate it
executes is the one `putnami.ci.json` declares — `lint`, `test`, `build`,
`validate`, scoped by `--impacted` — the same tasks as the command above. The
document (version 3, see `protocols/ci`) declares those `commands`, the rules
that name the channels a branch publishes and the environments that follow them
(`main` publishes the `canary` channel, and `prod` follows it). It carries no
job and no credential input, so a pull request is gated on exactly what you can
run locally. No rule matches a pull request, so its run stops after those
commands: packaging and publication run on `main` only. The generated agent
guidance derives its gate line from the same document.

On `main`, the same run continues into `putnami deploy --all` with a run-bound
Cloud capability. Deploy participation is an explicit opt-in: the workspace sets
`@putnami/cloud.deploy.enabled` to `false` in `putnami.workspace.json`, and only
the two deployed sites (`sites/putnami.dev`, `sites/telemetry.putnami.dev`) set
it to `true` in their `putnami.json`. Everything else is verified but never
packaged, published, or released by that run. Preview the exact DAG with
`putnami deploy --all --plan`; it must plan without overlapping outputs.

Beside it, [`.github/workflows/contributor-ci.yml`](.github/workflows/contributor-ci.yml)
runs the documented contributor path on every pull request, on a GitHub-hosted
runner that reads no secret and empties every Putnami credential variable before
starting. It complements `putnami ci` — the merge decision rests on `putnami ci`;
a red Contributor CI means the credential-free path broke and is worth fixing on
its own. It is scoped by `--impacted`, so a documentation-only pull request
selects zero projects and passes having run nothing; the job summary prints the
selected project and job counts so that case is visible.

The job publishes one service container: a `postgres:17-alpine` on port 6432,
which is the fallback datasource
[`typescript/framework/database/conf/.env.test.yaml`](typescript/framework/database/conf/.env.test.yaml)
names. `@putnami/database` plans no `test~test-env` task, so nothing provisions
it a database and its suite would otherwise start its own container mid-test.
The image is public and the credentials are the fixture's own, so the job still
holds nothing you do not.

## Repository Layout

The repo is organized by ownership — each language ecosystem is self-contained:

- `tooling/` — workspace-wide machinery: CLI, extension SDK, scaffold, client generator, tooling samples
- `intelligence/` — public Intelligence extensions, including the agent-readiness CLI and repository collector
- `protocols/` — cross-pillar wire contracts (schemas + fixtures) shared by frameworks, tooling, and platform
- `typescript/` — TypeScript extension, frameworks, templates, samples
- `go/` — Go extension, frameworks, templates, and samples (`go.putnami.dev/*`)
- `python/` — Experimental Python extension, templates, and samples; no shipped Python framework
- `sites/` — deployed sites: `putnami.dev` (documentation) and `telemetry.putnami.dev` (CLI-usage receiver)

Each language directory follows the same shape: `extension/`, `framework/`, `templates/`, `samples/`.

### Responsibilities

- Extension identity is manifest-driven and package-driven, not path-driven.
- `<language>/extension` owns generic lifecycle jobs: `build`, `test`, `lint`, `serve`, `package`.
- `<language>/framework/*` owns runtime APIs, templates, generators, and optional hooks.
- Framework packages must not compete with the language extension for generic lifecycle command names.
- Docs live next to the thing they describe. `sites/putnami.dev` aggregates docs across the repo, but does not own them.
- Language-owned sites live under their language root. Cross-domain sites live under `sites/`.

### Retired Roots

The following top-level roots are retired. Do not add tracked files under them:

`apps/`, `core/`, `extensions/`, `framework/`, `packages/`, `web/`, `samples/`, `scripts/`, `docs/`

Use introspection commands to understand the workspace:

```bash
putnami projects list
putnami projects describe <project-name>
putnami extensions list
```

## Code Style

- **Formatter/Linter**: Biome (not ESLint or Prettier)
- **Indentation**: 2 spaces
- **Tests**: Bun native test runner (`bun:test`), files named `*.test.ts` or `*.spec.ts` in `test/` directories

## Documentation

Documentation updates are required for any user-facing change, in the same PR.

Each package documents itself in its own `doc/` folder; `sites/putnami.dev`
publishes those folders and does not own their content. Site-local pages —
getting started, concepts, how-to guides, principles — live in
`sites/putnami.dev/doc/`. Which source produces which published section is
listed in [`sites/putnami.dev/README.md`](sites/putnami.dev/README.md).

A user-visible change also carries its product intent: a feature declaration in
`putnami.features.json`, one spec under `specs/`, and — when the change rests on
a durable choice — a decision record under `<project>/doc/adr/`. The workflow is
[Write a feature spec](https://putnami.dev/docs/how-to/write-a-feature-spec).
Never hand-write feature evidence; it is emitted by build and test producers.

A public support status is a separate, reviewed decision recorded only in
[`putnami.support.json`](putnami.support.json). Do not claim one in prose, and
do not add an entry without the review that `protocols/support` describes.

What earns each status, and what forces it back down, is
[`protocols/support/doc/adr/0002-promotion-and-demotion-criteria.md`](protocols/support/doc/adr/0002-promotion-and-demotion-criteria.md).
Promoting a subject to `stable` or withdrawing that promise also edits
`reviewedStableSubjects` in `protocols/support/conformance_test.go`, so the
change is reviewed for what it is rather than arriving inside an unrelated one.

## Dependency Management

Always use the `putnami` CLI for dependencies:

```bash
putnami deps add <package>
putnami deps add <package> --package <target-project>
putnami deps remove <package>
```

Never run `bun add`, `npm install`, or create lockfiles other than `bun.lock`.

## Reporting Issues

Open an issue on GitHub with:

- A clear description of the problem or suggestion
- Steps to reproduce (for bugs)
- Expected vs actual behavior
- Environment details (OS, Bun version)

## Code of Conduct

This project follows a [Code of Conduct](CODE_OF_CONDUCT.md). By participating, you agree to uphold it.

## Governance

[GOVERNANCE.md](GOVERNANCE.md) records who approves what, and how a rule gets
waived when a change cannot satisfy it. [RELEASING.md](RELEASING.md) records the
release cadence, the release checklist, and who can roll a release back. Read
them before proposing a change to a public promise: a support status, a version
window, or the license.

## License

By contributing, you agree that your contributions will be licensed under the project's [FSL-1.1-MIT License](LICENSE.md).
