# Node workspaces

The Node workspaces detector reads the packages of an npm, pnpm or yarn
workspace, which covers Turborepo, and Lerna when it runs on those workspaces.
It reads the repository's own
files and runs no package manager, no `turbo` and no `lerna`. Its answer is a
workspace probe result (`go.putnami.dev/protocol/workspace`), so it can move to
an extension of its own without a contract change.

The change analyses read its answer to map changed files, tests and coverage
to packages, find the packages that depend on a change, and diff the lockfile.

## The members

The members come from the first of:

1. `pnpm-workspace.yaml`, its `packages` list. A file that holds only settings
   declares no member.
2. The root `package.json`, its `workspaces` list, or the `packages` of a
   `workspaces` object (yarn).

A member is a directory that holds a `package.json` and that an including
pattern matches and no excluding pattern (`!`) does. A pattern takes `*`, `?`,
`[...]`, `{a,b}` and `**`, which matches any number of directories. `node_modules`
and `.git` are skipped, and a wildcard never matches a directory whose name
starts with a dot. The root is never a member. A repository where neither file
declares members is not a Node workspace. A Rush monorepo lists its projects in
`rush.json` instead; the [Rush detector](09-rush.md) reads it, and its lockfile
under `common/config/rush/`.

Each member is a component named by its directory, with its package name and
version.

## The edges

A member depends on another member when one of its `dependencies`,
`devDependencies`, `optionalDependencies` or `peerDependencies`:

- uses the `workspace:` protocol, by name, alias (`workspace:other@*`) or path;
- points to its directory with `file:`, `link:` or `portal:`;
- names it with a version range its version satisfies.

pnpm links a version range to a member only when `linkWorkspacePackages` is
`true` in `pnpm-workspace.yaml` or `link-workspace-packages=true` is in the root
`.npmrc`, as in pnpm 9 and later, so the detector counts a range only then. Any
other dependency is external. When two members share a package name, the first
directory in path order keeps it.

## The metadata

Each member carries this metadata in the probe result:

| Member | Meaning |
|---|---|
| `ecosystem` | Always `node`. |
| `manager` | `npm`, `pnpm` or `yarn`: the `packageManager` field, else `pnpm` when `pnpm-workspace.yaml` exists, else the manager of the lockfile, else `npm`. A bun workspace reads as `npm`. |
| `lockfile` | The root lockfile of that manager, else another root lockfile: `pnpm-lock.yaml`, `yarn.lock`, `package-lock.json` or `npm-shrinkwrap.json`. Absent when there is none. |
| `reports.junit` | Where the member writes JUnit XML by default, relative to its directory: `junit.xml`, `junit/*.xml`, `reports/junit.xml`, `reports/junit/*.xml`, `test-results/*.xml`. |
| `reports.coverage` | Where it writes coverage, relative to its directory: `lcov.info`, `coverage-final.json`, `cobertura-coverage.xml` and `clover.xml`, under `coverage/` or `reports/coverage/`. |

## Diagnostics

The detector never fails a run. A file it cannot read is a diagnostic, and it
answers with what it read:

| Code | Cause |
|---|---|
| `observe.node_manifest_invalid` | A `package.json` or `pnpm-workspace.yaml` that cannot be read or parsed: a member with one is left out. Or two members with one package name. |
| `observe.node_pattern_invalid` | A workspace pattern that is absolute, leaves the root, has unbalanced braces or more than 256 alternatives, or is not a valid glob. It is ignored. |
| `observe.node_walk_bounded` | The member walk stopped after 200 000 directories. Members past it are left out. |
