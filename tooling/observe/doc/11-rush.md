# Rush

The Rush detector reads the projects of a [Rush](https://rushjs.io) monorepo.
Rush lists its projects in `rush.json`, not in `package.json` `workspaces` or
`pnpm-workspace.yaml`, so the [Node workspaces detector](04-node-workspaces.md)
finds none. The detector reads `rush.json` and the projects' `package.json`
files, and runs no `rush`, no package manager, and reads no `common/temp` or
`node_modules`.

## The projects

A root `rush.json` makes the repository a Rush monorepo. The file is JSON with
comments: `//` and `/* */` comments and trailing commas are read. Each entry of
`projects` is a component:

| Member | Value |
|---|---|
| id | its `projectFolder` |
| name | its `packageName` |
| ecosystem | `node` |
| source | `orchestrator` |

An entry without a `packageName`, with a `projectFolder` that is absolute,
leaves the repository or is the root, or with a folder or package name an
earlier entry took, is a diagnostic and is left out.

## The edges

A project depends on another Rush project when one of the `dependencies`,
`devDependencies`, `optionalDependencies` or `peerDependencies` of its
`package.json` names that project's package name:

- with the `workspace:` protocol, an alias (`workspace:other@*`) included;
- with a version range the other project's `package.json` version satisfies,
  read as the [Node workspaces detector](04-node-workspaces.md#the-edges)
  reads ranges. An `npm:` alias is installed from the registry and is no edge,
  as in Rush.

A dependency whose key the project lists in `decoupledLocalDependencies`, or
in the older `cyclicDependencyProjects`, is installed from the registry and is
no edge. Any other dependency is external.

## The lockfiles

The package manager `rush.json` names selects the lockfile:

| `rush.json` | Lockfile |
|---|---|
| `pnpmVersion` | `common/config/rush/pnpm-lock.yaml` |
| `npmVersion` | `common/config/rush/npm-shrinkwrap.json` |
| `yarnVersion` | `common/config/rush/yarn.lock` |

In pnpm mode, each `common/config/subspaces/<name>/pnpm-lock.yaml` is a
lockfile too. The change analyses read them with the pnpm, npm and yarn
readers (see [Dependency changes](06-change-analyses.md#dependency-changes)).
Rush's temporary packages, named `@rush-temp/`, stand for the repository's own
projects, and `file:` or `link:` packages are local: neither is a dependency.
A lockfile lies outside every project, so its changes name no component.

## Diagnostics

The detector never fails a run:

| Code | Cause |
|---|---|
| `observe.rush_config_invalid` | A `rush.json` that cannot be read or parsed: the detector finds no project, and the other detectors answer for the repository. Or an invalid project entry, which is left out. |
| `observe.rush_manifest_invalid` | A project's `package.json` that is missing or cannot be parsed: the project stays a component without edges. |
