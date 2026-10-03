# Upgrade Putnami

You want to keep Putnami up to date — the CLI, extensions, and framework dependencies in your projects.

## One command

```bash
putnami upgrade
```

This keeps the provider-optional, cloudless-compatible `stable`/`latest`
upgrade path and upgrades everything in sequence:

1. **CLI** — downloads the latest release binary and refreshes shell completions for your current shell
2. **CLI handoff** — when a new CLI was installed, continues the remaining phases under that verified binary
3. **Extensions** — resolves latest compatible versions of installed extensions. `@putnami/python` remains an explicit experimental opt-in, not a default upgrade target.
4. **Templates** — updates workspace templates
5. **Framework dependencies** — pins `@putnami/*` packages (TypeScript) and `go.putnami.dev/*` modules (Go) to the selected release. Python has no Putnami framework package release; an explicitly enabled Python extension upgrades that workspace's own uv-resolved dependencies.
6. **Workspace installers** — materializes lockfiles and workspace state (`bun install`, `go work sync`, `uv lock`, etc.)

Use `--dry-run` first when you want to inspect the resolved release before any
file is changed:

```bash
putnami upgrade --channel canary --dry-run
```

The output shows what changed at each step:

```
  Putnami upgrade plan
  Selector: stable (channel)

  CLI
  Already up to date (1.2.0)

  Extensions
  ↑ @putnami/typescript: 1.2.0 → 1.3.0
  ✓ @putnami/go@2.1.0 (up to date)

  Templates
  ✓ typescript-web@1.0.0 (up to date)

  Dependencies
  Pinned go.work replace go.putnami.dev/app => go.putnami.dev/app v1.2.0
  Pinned @putnami/application: 1.1.0 -> 1.2.0
```

## Select a release

You normally select a release channel rather than typing a commit-derived
version:

```bash
putnami upgrade                    # stable/latest; no provider required
putnami upgrade --channel stable   # follow the stable channel on every registry
putnami upgrade --channel canary   # follow the canary channel on every registry
putnami upgrade --release rs_<64-hex> --namespace putnami # exact immutable release set
putnami upgrade --version 1.2.3    # exact version for CI or rollback
```

A channel is resolved on each ecosystem's **own** registry: an npm `dist-tag`,
a Go `@v/<channel>.info` version query on the module origin — resolved **per
module**, because one channel carries a different version per module — and the
channel projection of the put registry for the CLI, extensions, templates and
agent workflows. Every endpoint comes from the workspace's `registries` entry
for that ecosystem, so no URL is hard-coded and none is hand-written into a
dotfile. No release-set provider is involved and there is no namespace to
configure, so any workspace can follow a channel — not only the one that
publishes it. Omitting all selectors keeps the historical `latest` behavior; it
is intentionally not shorthand for explicit `--channel stable`.

There is no `--branch` selector: a branch is not a distribution channel. A
pull-request preview follows a `pr-<number>` channel a rule declares in
`putnami.ci.json`, like any other channel.

Each ecosystem resolves every coordinate before it writes anything, reports the
**source revision** of every resolved version — the trailing `-<sha>` segment of
an ordered pre-release, `-` for a stable one — and a failure names the package
or module, the channel, the registry host and the HTTP status. The Go query
carries the credential the `go` command would send to the origin, the
`machine <host>` entry of your netrc file, so a private origin answers it; an
anonymous refusal names the missing credential.

An npm package the registry does not have keeps its current version. So does
a package with no dist-tag for the channel. The upgrade prints a warning that
names each such package, and notes when it sent no credential. Any other npm
failure stops the upgrade before it writes anything: a refused credential, a
server error, or an unreadable packument (the package metadata document).

The dependency run stops at the first failing ecosystem, so a
channel npm serves and the Go origin does not moves neither; pass
`--continue-on-error` to let each ecosystem report its own outcome.

Because a channel is mutable, a channel that moves mid-run can mix two
publications across ecosystems. Use `upgrade --release <id>` when CI or a
rollback must pin one exact publication and must not consult a channel at all.
A release-set id is scoped by the namespace that published it, which your
workspace does not know, so you name it with `--namespace`. That path needs
exactly one installed release-set provider and fails closed without it. Each
library receives the exact version recorded for it, so a single upgrade may
legitimately install different versions across the workspace.

`--version` remains the compatible selector for a single exact Putnami version.
It does not reinterpret that version as a release-set id. Use one selector at a
time: channel for a moving stream, release for an immutable snapshot, or version
for a uniform exact version.

For the CLI and extension artifacts, `stable` maps to the registry's `latest`
tag — as it does for an npm dist-tag and a Go version query. The CLI and the
extensions are ordinary members of the release set: the release writes the put
registry's channel projection in the same transaction that advances the channel,
and the archive downloads carry your own credential, so a private channel is
reachable. A `401` or `403` names `putnami cloud login` rather than reporting a
missing release.

For Go workspaces, the selected release is written to root `go.work` as
`replace` directives:

```go
replace (
    go.putnami.dev/app => go.putnami.dev/app v1.2.0
    go.putnami.dev/http => go.putnami.dev/http v1.2.0
)
```

The upgrade also reconciles the framework modules present in the workspace's
final Go dependency graph. That means newly introduced transitive modules get
matching `go.work` replacements, malformed prerelease tails are normalized to
the selected release, and any managed `exclude` of that exact release is
removed before dependency resolution.

For TypeScript workspaces, the framework version lives in the Bun **catalog** —
the single source of truth. Packages reference `@putnami/*` with the `catalog:`
protocol, and `putnami upgrade --deps` pins the selected release in the catalog:

```json
{
  "workspaces": ["packages/*"],
  "catalog": {
    "@putnami/application": "1.2.0",
    "@putnami/web": "1.2.0"
  }
}
```

```jsonc
// packages/web/package.json — consumers reference the catalog
{
  "dependencies": {
    "@putnami/application": "catalog:",
    "@putnami/web": "catalog:"
  }
}
```

The catalog is updated in place:

- Non-Putnami catalog entries (`react`, `react-dom`, `@types/react`, …) are
  preserved.
- No duplicate root `dependencies` or `overrides` are introduced — the catalog
  is the only version policy. Any leftover `@putnami/*` entries in root
  `dependencies`/`overrides` are removed.
- A package referenced with `catalog:` but missing from the catalog is added
  automatically, so `bun install` always resolves.

Bun also accepts the catalog nested under `workspaces.catalog`; the upgrade
updates whichever placement your workspace uses. A workspace that predates
catalog support (no `catalog` declared) falls back to exact-pinned root
`dependencies` only. A same-version `override` duplicating a direct dependency
is redundant npm `EOVERRIDE` bait, so redundant `@putnami/*` overrides are
stripped rather than written — the direct dependency is the sole version policy,
consistent with the catalog path.

Local workspace packages are left alone. For example, a Putnami framework
checkout that has `@putnami/web` or `go.putnami.dev/http` locally will keep
using the local package instead of replacing it with a registry version.

Enabling the `@putnami/cloud` extension does not, by itself, pin the npm
`@putnami/cloud` package. The extension is resolved through the
lock/artifact-store path, and its optional `@putnami/cloud/runtime` package is
loaded lazily with graceful degradation when absent — so a hoisted root install
is not needed for compile or build. `@putnami/cloud` is managed only when a
project actually declares it as a dependency. If an app imports
`@putnami/cloud/runtime` (for a remote config or secrets source), add
`@putnami/cloud` to that app's own `dependencies`; `putnami upgrade --deps` then
pins it to the selected release like any other referenced package.

## Upgrade specific layers

Use flags to upgrade only what you need:

```bash
putnami upgrade --cli          # Only the CLI binary
putnami upgrade --extensions   # Only extensions and templates
putnami upgrade --deps         # Only framework dependencies
```

If `putnami.workspace.json` pins an extension or template to one exact release,
`putnami upgrade` and `putnami upgrade --extensions` write the new release to
that pin and to `putnami.lock.json`, so the two files agree.
`putnami extensions update` and `putnami templates update` do the same. Ranges,
channels, `latest`, unpinned entries and local paths stay as they are. A pin in
the global putnami config is not edited: the command prints a warning that
names it. If a template is declared more than once and the rewrite would leave
another entry in effect, the command stops before writing the lock and names
the template: keep one entry for it.

## Upgrade the global CLI

Inside a workspace, `putnami upgrade` updates the active CLI in
`.putnami/bin/`. To update the global install in `~/.putnami/bin/`
— the one the install script created — pass `--global`:

```bash
putnami upgrade --global                   # global CLI + the regular workspace upgrade
putnami upgrade --global --cli             # global CLI only
putnami upgrade --global --channel canary  # same, from the latest canary
```

`--global` retargets the CLI phase at the global install and replaces
re-running the install script. Inside a workspace, the remaining phases —
extensions, templates, and dependencies — still run, exactly like a plain
`putnami upgrade`. Outside a workspace it upgrades the CLI binary and shell
completions only (no workspace required). The release selectors (`--channel`,
`--version`) work the same as for a workspace upgrade. You only
need `install.sh` for the first install on a machine — what it verifies and how
it reaches your `PATH` is in [Getting Started](/docs/getting-started). An
explicit `--channel` requires a workspace release-set provider only when the
run includes dependencies; `--release` is dependency-only. The unqualified
global upgrade keeps the provider-optional compatibility path.

If `putnami.lock.json` pins a workspace CLI, that pin is not changed by
`upgrade`: it is a committed trust decision. A full upgrade hands its remaining
phases to the freshly verified binary to avoid mixing a new extension with an
older engine, then warns when later commands would return to a different pin.
To adopt the new CLI for ordinary workspace commands, run
`putnami pin <version>` deliberately and add a digest from each platform your
team uses.

## After upgrading

Verify everything still works:

```bash
putnami lint,test,build --impacted
```

If the upgrade included breaking changes, the build or test output will surface what needs updating. Use `--output=jsonl` for structured diagnostics if you need to pinpoint exact failures.

If you use zsh and completions still look stale after a CLI upgrade, clear zsh's
completion cache and restart the shell:

```bash
rm -f ~/.zcompdump*
exec zsh
```

## Low-level commands

The `putnami upgrade` command orchestrates these individual commands, which you can also run directly:

| What | Command |
|------|---------|
| CLI binary | `putnami upgrade --cli` (or `--global` for the global install) |
| Extensions | `putnami extensions update` |
| Templates | `putnami templates update` |
| Dependencies | Handled by extension `deps-upgrade` jobs |

### Go workspace and standalone dependency metadata

For Go projects, the root `go.work` and `go.work.sum` are the workspace release
lock. An upgrade changes those files without copying the same Putnami version
and checksums into every workspace member's `go.mod` and `go.sum`.

Member metadata is upgraded independently when a project declares the `go`
publish channel, because a published module must resolve without its source
workspace. A non-published application or tool that intentionally builds with
`GOWORK=off` can opt into the same behavior:

```json
{
  "options": {
    "@putnami/go": {
      "standalone": true
    }
  }
}
```

Use this option only for a real standalone boundary such as a packaged CLI or
an extension runtime. Ordinary Putnami build, test, lint, and serve jobs resolve
through the workspace lock and do not need duplicate member pins.

To switch the active CLI to an exact release, pass `--version`:

```bash
putnami upgrade --cli --version 1.2.3      # workspace pin
putnami upgrade --global --version 1.2.3   # global install
```

You now have a fully updated Putnami workspace — CLI, extensions, and project dependencies.
