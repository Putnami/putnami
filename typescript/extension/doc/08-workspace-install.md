# Workspace Install

The workspace-install command runs `bun install` at the workspace root to install all dependencies.

## Overview

- Installs dependencies for the entire workspace in a single operation
- Runs at the workspace level (not per-project)
- Supports forced reinstall when the lockfile is up to date
- Runs as a quiet command (suppressed from job recaps)

## Usage

```bash
# Install dependencies
putnami workspace-install

# Force reinstall (ignore lockfile check)
putnami workspace-install --force
```

## Which Bun runs

The install needs no Bun on the machine. It selects one in this order and
downloads only at the last step:

1. **A Bun the machine holds**: `bun` on `PATH`, then `bin/bun` under
   `$BUN_INSTALL`, then `~/.bun/bin/bun`. When the workspace names a release,
   only a Bun that reports exactly that release qualifies. When it names none,
   the first one found is used.
2. **A Bun that Putnami installed before**:
   `<Putnami home>/toolchains/bun/bun-<version>/bin/bun`. The Putnami home is
   `PUTNAMI_HOME`, else `~/.putnami`.
3. **A new install** of the release at that path.

The release is the one `putnami.lock.json` pins under `toolchains.bun`. Without
a pin it is the one the root `package.json` declares in `packageManager`.
Without either it is the extension's default release, Bun 1.4.0.

| The workspace | The install downloads | Checked against |
|---------------|-----------------------|-----------------|
| pins a release in the lock | `bun-<target>.zip` from the lock's `source` | the lock's SHA-256 for the host platform |
| names no release | the default release from the vendor's GitHub release | the SHA-256 the extension ships for each of the six platforms |
| declares a release the lock does not pin yet | nothing for that release: no SHA-256 can check it | see below |

`<target>` is `darwin-x64`, `darwin-aarch64`, `linux-x64`, `linux-aarch64`,
`windows-x64` or `windows-aarch64`.

A download is refused when the release has no SHA-256 for the host platform or
when the archive's SHA-256 differs. Nothing is extracted and no `bun` is left on
disk. The error names the release, the URL without its credentials, and the
command to run next. An archive entry that is a link, or that would land outside
the install directory, is refused the same way. Two installs of the same release
at the same time, from two workspaces, leave one complete install.

When the root `package.json` declares a release that the lock does not pin yet,
this install runs with a Bun of the machine of another release, else with the
default release, and logs a warning. `putnami install` pins the declared release
in the lock when it ends, and the next `putnami install` installs that release.

A Bun on the machine that differs from the pin does not block anything: the
pinned release is installed under the Putnami home and every task runs with it.
The task runtime of the extension lists the install as its last candidate.

A Bun that Putnami installed keeps its own files under its install directory.
`BUN_INSTALL` is `toolchains/bun/bun-<version>`, its package cache is
`install/cache` there, and its transpiler cache is `install/cache/@t@` there.
It writes nothing to `~/.bun`. A Bun the machine holds keeps its own locations.

The lock holds one SHA-256 for each operating system and architecture, which is
the glibc archive on Linux. On a musl host such as Alpine, put the musl build of
the release on `PATH`; the install refuses to download there.

A hosted run downloads no Bun: `workspace-fetch` and `workspace-install` run the
Bun the runner holds, at one of the three places above, and fail with a message
that names the release when there is none.

## Behavior

The command performs these steps:

1. **Ensure workspace devDependencies** — adds required devDependencies (`@biomejs/biome`, `@types/bun`, `typescript`) to the workspace root `package.json` if they are not already present. Existing versions are never overridden.
2. **List the workspace members** — writes the root `package.json` `workspaces` from the workspace membership, as `putnami projects sync` does: every member with a `package.json`, the workspace root excluded. A project created after the last `projects sync` is therefore installed, and a list that already matches is not rewritten. If the list cannot be read or written, the install fails before `bun install` runs.
3. **Seed catalog entries** — for any package a member references with Bun's `catalog:` protocol but that is missing from the root catalog, an entry is added (pinned to `latest`) so `bun install` can resolve it. `@putnami/*` framework packages scaffolded by the templates are seeded this way; `putnami upgrade --deps` later pins them to an exact release. Existing catalog entries — Putnami or otherwise — are never overwritten.
   When the install receives a channel other than `latest` through the `putnami-channel` job option, which `putnami init` sets for the channel it resolves on, a missing `@putnami/*` entry is the exact version that channel's dist-tag names for the package on the registry the workspace declares, never the channel name. A package the channel does not name fails the install before `bun install` runs; the seed does not read `latest` instead.
4. **Declare the bun version** — when the root `package.json` has no `packageManager` field, the command runs `bun --version` with the Bun it selected (see [Which Bun runs](#which-bun-runs)) and writes `"packageManager": "bun@<version>"`. `putnami install` pins the lock's bun toolchain from that field, and TypeScript tasks then run only with a bun of that exact version. An existing field is never changed. A bun whose version is not a plain release, such as a canary build, is not declared.
5. **Refresh the private-registry credential** — see below.
6. **Run `bun install`** — executes `bun install` in the workspace root directory. Bun resolves all `workspace:*` and `catalog:` references and creates symlinks between workspace packages.

When `--force` is not set, Bun skips installation if `bun.lock` is already up to date.

Downloaded packages are shared across repositories and worktrees through Bun's
machine-global cache. For a Bun the machine holds, that is
`~/.bun/install/cache`. For a Bun that Putnami installed, it is
`<Putnami home>/toolchains/bun/bun-<version>/install/cache`.
`PUTNAMI_BUN_CACHE_DIR` or `BUN_INSTALL_CACHE_DIR` names another directory for
both. Each worktree keeps its own `node_modules`, materialized efficiently from
that cache with clonefiles or hardlinks. This extension owns the lifecycle of
those caches: `putnami cache gc` fans out to its `cache-gc` command, which
bounds each of them to 10 GiB by default. CI should persist the cache directory
rather than `node_modules`.

## Private registries: credentials at install time

`@putnami/*` packages live on a private registry. Before `bun install` runs, the
extension asks `@putnami/cloud` to write your npm credential for that registry:

1. It reads the `@putnami:registry=` line from the workspace `.npmrc` — the line
   step 5 above maintains — and takes its host.
2. It runs `putnami cloud registry-token --host <host> --materialize`, which
   writes the `//<host>/:_authToken=` line in your own `~/.npmrc`.

The extension never sees the token, and the host is the whole request: no
package name and no action cross the seam. `putnami upgrade --deps` does the
same refresh before it reads registry metadata.

The refresh never blocks an install. If `@putnami/cloud` is not installed, if
you are not signed in (`run putnami cloud login`), or if the registry is not an
`https` host, the install logs one line and runs with whatever credential your
machine already has.

## Hosted runs: fetch, then install

On a hosted run, no process the repository controls can read a registry
credential. A package's install script is such a process, so the download and
the install are two jobs:

1. **`workspace-fetch`** downloads what the committed `bun.lock` names into
   bun's cache. It is the only job the engine hands the read credential to.
2. **`workspace-install`** then installs from that cache. The engine marks every
   job of a hosted run with `PUTNAMI_OFFLINE_DEPENDENCIES=1`; with that mark the
   install runs `bun install --frozen-lockfile` (plus `--force` when asked) and
   refreshes no credential. Outside a hosted run it keeps the steps above
   unchanged.

On a hosted run, neither command shapes the workspace manifests (steps 1 to 4
above, `tsconfig.json`, `biome.json` and the `.npmrc` scopes): both read the
committed files as they are, so the install finds the manifests the fetch
downloaded for, and no committed file changes. A manifest the committed
`bun.lock` does not match fails the frozen install; run `putnami install`
locally and commit the result.

`workspace-fetch` works as follows:

1. It reads the credential from the descriptor the engine hands it, before it
   starts any process, so no process inherits that descriptor.
2. It requires a committed `bun.lock`. Without one it fails with "a hosted run
   installs from a committed bun.lock; run `putnami install` locally and commit
   it". It never writes the lock: a manifest the lock no longer matches fails
   the fetch.
3. It copies what a frozen install reads into a private temporary directory:
   the root and member `package.json` files, `bun.lock`, `bunfig.toml`, the
   patch files `patchedDependencies` names, the local `file:` dependencies,
   and the workspace `.npmrc`. When a credential is handed, the copied
   `.npmrc` loses its `_authToken`, `_auth` and `_password` lines, which would
   otherwise replace the handed bearer. A `link:` dependency, or a local
   dependency or patch outside the workspace, fails the fetch: a hosted run
   fetches only what the workspace holds.
4. It writes `//<host>/:_authToken=<bearer>` for each host of the credential in
   a `.npmrc` (mode 0600) in a private home, and points `HOME` and
   `XDG_CONFIG_HOME` there. The bearer is in that file only, never in an
   environment variable.
5. It runs `bun install --ignore-scripts --frozen-lockfile` in the copy, with
   `BUN_INSTALL_CACHE_DIR` set to the cache `bun pm cache` reports for the
   workspace, which is the cache `workspace-install` reads. No package script
   runs, and the workspace gets no `node_modules`.
6. It removes the copy and the private home when bun exits, also when the
   install fails or the job is stopped with SIGINT or SIGTERM. A SIGKILL leaves
   them in the temporary directory.

Outside a hosted run the engine hands no descriptor, and `workspace-fetch`
refreshes and uses your machine's own credentials, as `workspace-install` does.
On a hosted run whose credential-provider holds no read credential, or that has
no credential-provider, the engine hands a descriptor without one. The fetch then uses the credentials the machine
already has and refreshes none: it starts no `putnami cloud registry-token`,
which would run a CLI that does not hold the run credential.

The fetch runs in a copy because bun 1.4 has no offline install: it accepts
`--offline` and ignores it. An `--ignore-scripts` install in the workspace would
leave a `node_modules` that the next install treats as complete, so the trusted
install scripts would never run.

## API Reference

### Flags

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--force` | `boolean` | `false` | Force reinstall even if `bun.lock` is current |

## Boundaries

- **Scope**: Running `bun install` at workspace root, and on a hosted run
  fetching the locked packages into bun's cache first
- **Out of scope**: Per-project dependency management, lockfile generation, package resolution
- **Dependencies**: None on the machine. The command installs Bun when the
  machine holds none that fits
- **Extension points**: None
