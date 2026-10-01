# TypeScript Getting Started

Use TypeScript when the first project is a web app, an API service, or a shared package that should live inside the same Putnami workspace graph as everything else.

This path gives you a runnable project first, then points you to the framework primitives that matter next.

## Prerequisites

The gated `typescript-web` starter requires [Bun](https://bun.sh/) v1.4.0 or
later — the workspace-declared toolchain baseline recorded in
`package.json#packageManager` and `putnami.lock.json`.

## Pick the project shape

| You need... | Template | What you get |
|-------------|----------|--------------|
| A React SSR application | `typescript-web` | File-based pages, layouts, loaders, actions, islands, static assets |
| An HTTP API service | `typescript-server` | API routing, handlers, tests, application runtime |
| A shared TypeScript package | `typescript-library` | Package exports, tests, workspace dependency wiring |

If you are starting from an empty directory:

```bash
putnami init
```

## Create the project

For a web app:

```bash
putnami projects create web --template typescript-web
putnami deps install
putnami serve web
```

For an API service:

```bash
putnami projects create api --template typescript-server
putnami deps install
putnami serve api
```

For a library:

```bash
putnami projects create ui --template typescript-library
putnami deps install
putnami test ui
```

## What to look at first

Start with the project shape, then add framework depth only when the app needs it:

1. [Web](/docs/frameworks/typescript/web) for React SSR, routing, loaders, actions, and islands.
2. [API](/docs/frameworks/typescript/api) for HTTP endpoints and server-side handlers.
3. [Configuration](/docs/frameworks/typescript/configuration) when the first environment variable appears.
4. [Dependency injection](/docs/frameworks/typescript/dependency-injection) before runtime wiring becomes implicit.
5. [Testing](/docs/frameworks/typescript/testing) before the project becomes a dependency for another project.

## Daily loop

```bash
putnami serve web
putnami test web
putnami lint,test,build --impacted
```

`--impacted` keeps the feedback loop scoped to the projects affected by your branch.

## Read next

- [TypeScript overview](/docs/frameworks/typescript/overview) for the package map and runtime model.
- [Extension](/docs/frameworks/typescript/extension) for build, test, lint, serve, publish, and Docker behavior.
- [Tooling & Workspace](/docs/tooling-&-workspace) for the shared graph and CLI model.
