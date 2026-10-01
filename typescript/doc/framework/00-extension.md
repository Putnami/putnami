# TypeScript Extension

The `@putnami/typescript` extension is the TypeScript project lifecycle inside a Putnami workspace. It owns project detection, dependency setup, generation, build, test, lint, serve, packaging, and TypeScript-specific publish behavior.

Use this page as the map. The detailed lifecycle is split by phase so you can jump to the part of the pipeline you are debugging or configuring.

## Extension phases

| Phase | Start here | What it explains |
|-------|------------|------------------|
| Project setup | [Project detection](/docs/frameworks/typescript/extension/project-detection) | Which projects the extension owns and how templates activate it |
| Dependencies | [Dependencies & install](/docs/frameworks/typescript/extension/dependencies-and-install) | Bun, Biome, workspace install, and extension dependencies |
| Generation | [Generate phase](/docs/frameworks/typescript/extension/generate-phase) | Pre-build hooks, generated exports, and generated assets |
| Feedback loop | [Build, test & lint](/docs/frameworks/typescript/extension/build-test-lint) | Compile phases, Bun tests, Biome format/lint |
| Local runtime | [Serve & watch](/docs/frameworks/typescript/extension/serve-and-watch) | Entrypoints, ports, hot reload, and multi-service mode |
| Release artifacts | [Publish & Docker](/docs/frameworks/typescript/extension/publish-and-docker) | npm packages, archives, Docker images, and stable releases |
| Repeatability | [Configuration & caching](/docs/frameworks/typescript/extension/configuration-and-caching) | Cache inputs, task dependencies, and project options |

## Enable the extension

Most TypeScript templates enable it for you:

```bash
putnami projects create web --template typescript-web
putnami deps install
```

To add it to an existing workspace:

```bash
putnami deps add @putnami/typescript
putnami deps install
```

The extension is auto-discovered from workspace dependencies and activates on TypeScript projects with the matching package metadata.

## External tools

- [Bun](https://bun.sh) for runtime, bundling, tests, and executable compilation
- [Biome](https://biomejs.dev) for formatting and linting
- TypeScript declaration generation for library and application packages

## Daily loop

```bash
putnami serve web
putnami test web
putnami lint,test,build --impacted
```

Use the phase pages when a command becomes unclear. The sidebar keeps `Getting Started`, `How To / Guides`, `Extension`, and `Framework / Capabilities` visible so readers can move between task-oriented and reference-oriented material.
