# ADR 0007 — The repository chooses its package manager

- **Status**: accepted
- **Scope**: `@putnami/typescript` (`typescript/extension`)

## Context

Workspace install ran `bun install` in every workspace. A Node repository
that adopts Putnami through `putnami init --from nx|turbo|moon --mode import`
then got a root `workspaces` list and a `bun.lock`, and its `pnpm-lock.yaml`,
`yarn.lock` or `package-lock.json` stopped being the one that pinned its
dependencies. For a team that has not chosen Bun, switching the installer is
a migration of its own, which kept adoptions in delegate mode.

The installer and the task runtime are two separate choices. Bun runs the
TypeScript tasks (`bun test`, `bun build`); the installer only has to put a
`node_modules` tree where those tasks resolve packages, and every one of
npm, pnpm, Yarn and Bun can do that from the repository's own lockfile.

## Decision

Workspace install runs the package manager the repository declares: the
`packageManager` field of the root `package.json`, else the manager whose
lockfile lies at the root. A workspace that declares neither installs with
Bun, so a workspace `putnami init` creates is unchanged.

- npm, pnpm and Yarn run from `PATH`, the way `turbo` does in delegate mode:
  the repository already provides them, through Corepack or a global
  install. The CLI provisions only Bun, the task runtime.
- Each manager reads its members and its catalog where it reads them:
  `pnpm-workspace.yaml` for pnpm, the root `package.json` otherwise. A
  manager without catalogs (npm, Yarn) gets no catalog seed.
- Every manager's lockfile is a workspace input and an install input, so a
  task's cache key follows the lockfile in use.
- A hosted run keeps its two steps for every manager: workspace-fetch fills
  the manager's own cache from a scratch copy of the committed manifests,
  with the bearer only in a private home and no repository code running, and
  workspace-install installs frozen and offline from that cache.

## Invariants

- No install writes the lockfile of a manager the repository does not use.
- A workspace that declares no manager installs with Bun.
- Bun stays the runtime of every TypeScript task, whatever the installer.
- workspace-fetch runs no code the repository holds: no manager binary inside
  the workspace, no Yarn `yarnPath` release, no Yarn plugin, no pnpmfile and
  no lifecycle script.

## Rejected alternatives

- **Keep Bun as the only installer.** The adoption then costs a lockfile
  migration before Putnami runs a single task.
- **Pin npm, pnpm and Yarn in the workspace lock, as Bun is.** It needs a
  toolchain resolver and digests for each manager, while the repository
  already brings its manager; a later change can add it.
- **A Putnami option that names the installer.** The repository already
  declares it in `packageManager` and its lockfile; a second declaration
  could only disagree with the first.
- **Run a Yarn `yarnPath` release in workspace-fetch.** It is a file the
  repository commits, run next to the registry credential.

## Consequences

- An adopted pnpm, npm or Yarn workspace keeps its lockfile, and its own
  `install` keeps working beside `putnami install`.
- A Yarn 2+ workspace whose `nodeLinker` is `pnp` installs without a
  `node_modules` tree, which the Bun tasks need; it sets `nodeLinker:
  node-modules`.
- The hosted runner rewrites only `bun.lock` registry URLs today; a hosted
  run of another manager reaches the registries its lockfile names.
