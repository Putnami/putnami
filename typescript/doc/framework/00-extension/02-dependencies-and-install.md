# Dependencies & Install

The TypeScript extension keeps JavaScript and TypeScript dependency setup workspace-native. You should use Putnami dependency commands, not package-manager commands directly.

## Install the extension

```bash
putnami deps add @putnami/typescript
putnami deps install
```

`putnami deps install` installs workspace dependencies and keeps project links consistent with the Putnami graph.

## Toolchain

The extension uses:

- Bun for install, runtime, bundling, tests, and executable compilation
- Biome for format and lint
- TypeScript for declaration output

The workspace install phase ensures required TypeScript dev dependencies exist at the workspace root when needed.

## When to run install

Run dependency install after:

- creating a TypeScript project from a template
- adding or removing a workspace dependency
- changing extension dependencies
- pulling a branch that changed the lockfile or workspace package metadata

```bash
putnami deps install
```

## Upgrading Putnami packages

`putnami upgrade --deps` pins `@putnami/*` packages to the selected release in
the workspace root `package.json`. It updates both `dependencies` and
`overrides` so direct and transitive Putnami packages resolve to the same
version before the workspace install runs.

Workspaces that enable the `@putnami/cloud` extension also have the npm
`@putnami/cloud` runtime package pinned during `putnami upgrade --deps`, even
before an app adds it directly.

## Project dependencies

Workspace dependencies should stay visible to Putnami. That lets `--impacted` select the right build, test, and lint jobs.

Use project references and workspace package metadata rather than hidden local imports. If one project consumes generated assets from another, keep that relationship in the normal workspace dependency graph.
