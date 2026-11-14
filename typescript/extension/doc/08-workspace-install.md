# Workspace Install

The workspace-install command installs all dependencies at the workspace root
with the package manager the repository declares: Bun, npm, pnpm or Yarn.

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

## Which package manager installs

The repository chooses its installer, so a Node repository that adopts Putnami
keeps its lockfile:

1. The `packageManager` field of the root `package.json`, such as
   `pnpm@10.26.1`, names it: `bun`, `npm`, `pnpm` or `yarn`. Any other name
   fails the install.
2. Without that field, a file at the workspace root names it, looked for in
   this order: `bun.lock` or `bun.lockb` (Bun), `pnpm-lock.yaml` or
   `pnpm-workspace.yaml` (pnpm), `yarn.lock` (Yarn), `package-lock.json` or
   `npm-shrinkwrap.json` (npm). After the Bun locks, this is the order the
   Nx, Turborepo and moon probes read, so an import and the install name the
   same manager.
3. With none, the install runs Bun. A workspace `putnami init` creates
   installs with Bun.

Yarn is Yarn 2 or later when the declared version is, or, without a version,
when the root holds a `.yarnrc.yml` or a `yarn.lock` in the Yarn 2 format.

npm, pnpm and Yarn run from `PATH`, through Corepack (`corepack enable`) or a
global install. The CLI provisions only Bun; when `PATH` holds no npm, pnpm or
Yarn the workspace declares, the install fails with a message that names it.

The installer only installs. Bun runs every TypeScript task (`bun test`,
`bun build`, the type check and Biome) whatever the installer, so the Bun of
the next section is needed in every TypeScript workspace. A Yarn 2 or later
workspace sets `nodeLinker: node-modules` in `.yarnrc.yml`: Bun resolves
packages from `node_modules`, which Plug'n'Play does not write.

Every manager's lockfile and `pnpm-workspace.yaml` are inputs of the TypeScript
tasks and install inputs of the workspace: a change to the lockfile in use
invalidates the tasks' cache and makes `putnami` install again.

## Which Bun runs

The extension installs no Bun. `putnami install` provisions the Bun release
the workspace lock pins under the Putnami home, and every job, workspace-install
and workspace-fetch of a Bun workspace included, runs the bun the CLI resolved
for the extension's task runtime. The task runtime takes the first of these candidates that reports
the pinned release:

1. `bun` on `PATH`.
2. `bin/bun` under `$BUN_INSTALL`.
3. `~/.bun/bin/bun`.
4. The release the CLI provisioned:
   `<Putnami home>/toolchains/bun/bun-<version>/bun-<target>/bun`, the layout
   of the vendor's archive. The Putnami home is `PUTNAMI_HOME`, else
   `~/.putnami`.

`<target>` is `darwin-x64`, `darwin-aarch64`, `linux-x64`, `linux-aarch64`,
`windows-x64` or `windows-aarch64`. A Bun on the machine of another release
does not block anything: the CLI passes over it for the provisioned release.

A job that starts with no bun on `PATH`, in `$BUN_INSTALL` or in `~/.bun` runs
the release from `toolchains/bun/bun-<version>` under the Putnami home, at
`bun-<target>/bun` or at `bin/bun`. The release is the one `putnami.lock.json`
pins under `toolchains.bun`, else the one the root `package.json` declares in
`packageManager`, else the extension's default release, Bun 1.4.0. When that
folder holds no Bun, the job fails with a message that names the release and
`putnami install`. It downloads nothing, on a hosted run or not.
`workspace-fetch` never runs a bun inside the workspace.

The extension answers the CLI's `__putnami toolchain-resolve` call for the lock
identity `bun`, which is how `putnami install` pins the release it provisions:

| The workspace | The answer |
|---------------|------------|
| declares no release in `packageManager` | the default release, offline, with the SHA-256 the extension ships for each of the six platforms |
| declares the default release | the same pin |
| declares another release | the SHA-256 digests that release's metadata on GitHub lists; offline, a problem whose remedy is `putnami install` with network access |
| commits a lock version 5 pin of that release | the committed pin, as it is |

The answer names each platform's archive URL, format, and executable beside its
SHA-256.

A Bun under the Putnami home keeps its own files in its release folder.
`BUN_INSTALL` is `toolchains/bun/bun-<version>`, whether the program is at
`bun-<target>/bun` or at `bin/bun`. Its package cache is `install/cache` there,
and its transpiler cache is `install/cache/@t@` there. It writes nothing to
`~/.bun`. A Bun the machine holds keeps its own locations.

The lock pins one archive for each operating system and architecture, which is
the glibc build on Linux. On a musl host such as Alpine, put the musl build of
the release on `PATH`.

## Behavior

The command performs these steps:

1. **Create the root `package.json`** — when the file is missing, writes `{"name": <workspace name>, "private": true, "workspaces": []}`, without `workspaces` for pnpm, which lists its members in `pnpm-workspace.yaml`. The name is the `name` in `putnami.workspace.json`, else the name of the workspace root directory. An existing file is never touched. `putnami projects sync` still creates no root `package.json`, so a Go-only or Python-only workspace gets none.
2. **Ensure workspace devDependencies** — adds to the workspace root `package.json` each devDependency a project's job needs, when it is not already present: `@biomejs/biome` for a lint with Biome; `typescript` and `@types/bun` for a build, a package of a project whose build the extension runs, and a test that type-checks; `typescript` alone for `validate`, which reads the API with its parser. A project that disables a job, and a test with `types: false`, need nothing for it. Existing versions are never overridden. Biome is added at the exact release that the `$schema` of the shipped Biome rules names, never as a range.
3. **List the workspace members** — writes the workspace membership where the package manager reads it, as `putnami projects sync` does: the `packages` list of `pnpm-workspace.yaml` for pnpm, keeping every other line of the file, and the root `package.json` `workspaces` otherwise. It lists every member with a `package.json`, the workspace root excluded, as exact paths in place of the patterns the file held: a workspace package that is no Putnami project leaves the manager's workspace, and the monorepo tool's, so every package of an imported repository is a Putnami project. A project created after the last `projects sync` is therefore installed, and a list that already matches is not rewritten. If the list cannot be read or written, the install fails before the package manager runs.
4. **Seed catalog entries** — with Bun or pnpm, the managers that read the `catalog:` protocol, for any package a member references through it but that is missing from the catalog, an entry is added (pinned to `latest`) so the install can resolve it: in the root `package.json` for Bun, and in the `catalog` or `catalogs` of `pnpm-workspace.yaml` for pnpm. npm and Yarn read no catalog and get no entry. `@putnami/*` framework packages scaffolded by the templates are seeded this way; `putnami upgrade --deps` later pins them to an exact release. Existing catalog entries — Putnami or otherwise — are never overwritten.
   When the install receives a channel other than `latest` through the `putnami-channel` job option, which `putnami init` sets for the channel it resolves on, a missing `@putnami/*` entry is the exact version that channel's dist-tag names for the package on the registry the workspace declares, never the channel name. A package the channel does not name fails the install before the package manager runs; the seed does not read `latest` instead.
5. **Declare the bun version** — in a workspace that installs with Bun, when the root `package.json` has no `packageManager` field, the command runs `bun --version` with the bun it runs (see [Which Bun runs](#which-bun-runs)) and writes `"packageManager": "bun@<version>"`. `putnami install` pins the lock's bun toolchain from that field, and TypeScript tasks then run only with a bun of that exact version. An existing field is never changed. A bun whose version is not a plain release, such as a canary build, is not declared.
6. **Write the root configuration a job reads** — writes a root `tsconfig.json` that extends the extension's when the root holds none and a project runs a build, a package with that build or a test that type-checks, which resolve the project's `tsconfig` and fall back to the root's, or holds a `tsconfig.json` that extends the root's. It writes a root `biome.json` from the shipped rules when the root holds none and a project lints with Biome. A lint with ESLint and `validate` read neither. An existing file is never touched.
7. **Refresh the private-registry credential** — see below.
8. **Install** — runs the package manager's install in the workspace root directory: `bun install`, `npm install`, `pnpm install` or `yarn install` (`--non-interactive` on Yarn 1). The manager resolves the `workspace:` and `catalog:` references, links the workspace packages, and writes its own lockfile; no other manager's lockfile is written.

`--force` adds the manager's own reinstall flag: `--force` for Bun, npm, pnpm
and Yarn 1. Yarn 2 and later has none and runs a plain install.

`putnami upgrade --deps` pins the `@putnami/*` versions in the catalog the
workspace declares, the root `package.json` one, else the `pnpm-workspace.yaml`
one, in place, else in the root `dependencies`. It then reinstalls with the same
package manager and its reinstall flag, and a failed install restores the root
`package.json`, `pnpm-workspace.yaml` and that manager's lockfiles.

Downloaded packages are shared across repositories and worktrees through Bun's
machine-global cache. For a Bun the machine holds, that is
`~/.bun/install/cache`. For a Bun under the Putnami home, it is
`<Putnami home>/toolchains/bun/bun-<version>/install/cache`.
`PUTNAMI_BUN_CACHE_DIR` or `BUN_INSTALL_CACHE_DIR` names another directory for
both. Each worktree keeps its own `node_modules`, materialized efficiently from
that cache with clonefiles or hardlinks. This extension owns the lifecycle of
those caches: `putnami cache gc` fans out to its `cache-gc` command, which
bounds each of them to 10 GiB by default. CI should persist the cache directory
rather than `node_modules`.

## Private registries: credentials at install time

`@putnami/*` packages live on a private registry. Before the install runs, the
extension asks `@putnami/cloud` to write your npm credential for that registry:

1. It reads the `@putnami:registry=` line from the workspace `.npmrc` — the line
   the install writes from the workspace's `registries.npm.scopes` — and takes its host.
2. It runs `putnami cloud registry-token --host <host> --materialize`, which
   writes the `//<host>/:_authToken=` line in your own `~/.npmrc`.

The extension never sees the token, and the host is the whole request: no
package name and no action cross the seam. `putnami upgrade --deps` does the
same refresh before it reads registry metadata.

The refresh never blocks an install. If `@putnami/cloud` is not installed, if
you are not signed in (`run putnami cloud login`), or if the registry is not an
`https` host, the install runs with whatever credential your machine already
has. Most installs fetch public packages only and need no credential, so the
install does not warn about the missing one up front. It holds the warning and
logs it only when the registry that holds the credential answers a request with
HTTP 401 or 403. The
warning names the missing credential: the sign-in hint, or, for a session that
holds only `PUTNAMI_CLOUD_TOKEN` and cannot sign in, the cloud's reason.

## Hosted runs: fetch, then install

On a hosted run, no process the repository controls can read a registry
credential. A package's install script is such a process, so the download and
the install are two jobs:

1. **`workspace-fetch`** downloads what the committed lockfile names into the
   package manager's cache. It is the only job the engine hands the read
   credential to.
2. **`workspace-install`** then installs from that cache. The engine marks every
   job of a hosted run with `PUTNAMI_OFFLINE_DEPENDENCIES=1`; with that mark the
   install runs frozen, and offline where the manager can, and refreshes no
   credential. Outside a hosted run it keeps the steps above unchanged.

| Manager | Hosted install |
|---------|----------------|
| Bun | `bun install --frozen-lockfile`, plus `--force` when asked |
| npm | `npm ci --offline` |
| pnpm | `pnpm install --frozen-lockfile --offline`, plus `--force` when asked |
| Yarn 1 | `yarn install --non-interactive --frozen-lockfile --offline`, plus `--force` when asked |
| Yarn 2 and later | `yarn install --immutable`, with `YARN_ENABLE_NETWORK=0` |

On a hosted run, neither command shapes the workspace manifests (steps 1 to 6
above and the `.npmrc` scopes): both read the
committed files as they are, so the install finds the manifests the fetch
downloaded for, and no committed file changes. A manifest the committed
lockfile does not match fails the frozen install; run `putnami install`
locally and commit the result.

`workspace-fetch` works as follows:

1. It reads the credential from the descriptor the engine hands it, before it
   starts any process, so no process inherits that descriptor.
2. It runs no code the repository holds. A package manager inside the
   workspace fails the fetch before any process starts, and so does, for
   Yarn, a `.yarnrc.yml` or `.yarnrc` at the root or in a directory above it
   (the home excepted) that mentions `yarnPath`, `yarn-path` or `plugins`, in
   any letter case and even in a comment, or holds a backslash; and, for pnpm,
   a `pnpm-workspace.yaml` or `.npmrc` that mentions `configDependencies` or a
   pnpmfile, or holds a backslash. The check reads the text, so no quoted,
   escaped or indented spelling of a key slips past it. Every process the
   fetch starts, its cache probes in the workspace root included, also runs
   with `YARN_IGNORE_PATH=1` or `npm_config_ignore_pnpmfile=true`, with
   `COREPACK_ENV_FILE=0`, so a Corepack shim reads no `.corepack.env` from the
   workspace, and without `COREPACK_ENABLE_UNSAFE_CUSTOM_URLS`. Declare the Yarn release in
   `packageManager` instead of `yarnPath`; Corepack provides it.
3. It requires the committed lockfile of the package manager: `bun.lock` for
   Bun, `pnpm-lock.yaml`, `yarn.lock`, or `package-lock.json` or
   `npm-shrinkwrap.json`. Without it the fetch fails with "a hosted run installs
   from a committed <lockfile>; run `putnami install` locally and commit it".
   It never writes the lock: a manifest the lock no longer matches fails the
   fetch.
4. It copies what a frozen install reads into a private temporary directory:
   the root and member `package.json` files, the lockfile, the manager's
   configuration (`bunfig.toml`, `pnpm-workspace.yaml` or `.yarnrc`), the
   patch files the workspace names (`patchedDependencies` for Bun and pnpm,
   `.yarn/patches` for Yarn 2 and later), the local `file:` dependencies, and
   the workspace `.npmrc` and `.yarnrc.yml`. The members are those the
   workspace lists and those the lockfile records. When a credential is
   handed, the copied `.npmrc` loses its `_authToken`, `_auth` and `_password`
   lines, and the copied `.yarnrc.yml` every line that mentions `npmAuthToken`
   or `npmAuthIdent`, which would otherwise replace the handed bearer. A local dependency
   or patch outside the workspace fails the fetch, and so does a `link:`
   dependency of a Bun workspace: a hosted run fetches only what the workspace
   holds.
5. It writes `//<host>/:_authToken=<bearer>` for each host of the credential in
   a `.npmrc` (mode 0600) in a private home, and for Yarn 2 and later the same
   hosts as `npmRegistries` entries of a `.yarnrc.yml` there, since that Yarn
   reads no `.npmrc`. It points `HOME` and `XDG_CONFIG_HOME` there, and sets
   `NPM_CONFIG_USERCONFIG` and `npm_config_userconfig` to that `.npmrc`, so a
   user configuration the job's environment names elsewhere does not replace
   it. `COREPACK_HOME` keeps naming the user's Corepack folder, so the release
   Corepack runs is not downloaded again. The bearer is in those files only,
   never in an environment variable.
6. It runs a frozen install with no lifecycle script in the copy, writing into
   the cache the manager reports for the workspace, which is the cache
   `workspace-install` reads:

   | Manager | Scratch install | Cache |
   |---------|-----------------|-------|
   | Bun | `bun install --ignore-scripts --frozen-lockfile` | `BUN_INSTALL_CACHE_DIR` from `bun pm cache` |
   | npm | `npm ci --ignore-scripts --no-audit --no-fund` | `--cache` from `npm config get cache` |
   | pnpm | `pnpm fetch --ignore-scripts`, which reads only the lock, so a pnpmfile checksum it records needs no pnpmfile | `--store-dir` from `pnpm store path` |
   | Yarn 1 | `yarn install --frozen-lockfile --ignore-scripts --non-interactive` | `--cache-folder` from `yarn cache dir` |
   | Yarn 2 and later | `yarn install --immutable --mode=skip-build` (`--skip-builds` on Yarn 2), with `YARN_ENABLE_SCRIPTS=0` | `YARN_CACHE_FOLDER`, `YARN_GLOBAL_FOLDER` and `YARN_ENABLE_GLOBAL_CACHE` from `yarn config get` |

   No package script runs, and the workspace gets no `node_modules`.
7. It removes the copy and the private home when the install exits, also when the
   install fails or the job is stopped with SIGINT or SIGTERM. A SIGKILL leaves
   them in the temporary directory.

Outside a hosted run the engine hands no descriptor, and `workspace-fetch`
refreshes and uses your machine's own credentials, as `workspace-install` does.
On a hosted run whose credential-provider holds no read credential, or that has
no credential-provider, the engine hands a descriptor without one. The fetch then uses the credentials the machine
already has and refreshes none: it starts no `putnami cloud registry-token`,
which would run a CLI that does not hold the run credential.

The hosted runner rewrites the registry URLs of `bun.lock` only; a hosted run of
another manager reaches the registries its lockfile names.

The fetch runs in a copy because bun 1.4 has no offline install: it accepts
`--offline` and ignores it. An `--ignore-scripts` install in the workspace would
leave a `node_modules` that the next install treats as complete, so the trusted
install scripts would never run.

## API Reference

### Flags

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--force` | `boolean` | `false` | Force a reinstall with the package manager's own flag, even when the lockfile is current |

## Boundaries

- **Scope**: Running the install of the package manager the repository
  declares at the workspace root, and on a hosted run fetching the locked
  packages into that manager's cache first
- **Out of scope**: Per-project dependency management, lockfile generation, package resolution
- **Dependencies**: Bun, which `putnami install` provisions under the Putnami
  home, and the npm, pnpm or Yarn the repository declares, on `PATH`; the
  command installs none
- **Extension points**: None
