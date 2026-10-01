# Getting Started

`@putnami/typescript` is the TypeScript extension for Putnami workspaces. It provides build, test, lint, serve, and package commands using [Bun](https://bun.sh/) as the runtime and [Biome](https://biomejs.dev/) for formatting and linting.

## Overview

- Compiles TypeScript libraries and applications with a 4-phase build pipeline
- Runs tests with Bun's built-in test runner, JUnit reporting, and LCOV coverage
- Formats and lints code with Biome (zero-config defaults included)
- Serves applications with hot-reload and structured log forwarding
- Packages npm tarballs and Docker images for deployment

## Prerequisites

- [Bun](https://bun.sh/) v1.4.0 or higher
- A Putnami workspace (created with `putnami init`)

## Installation

Add the extension to your workspace:

```bash
putnami deps add @putnami/typescript
```

Then declare it in each project's `putnami.json`:

```json
{
  "extensions": ["@putnami/typescript"]
}
```

Templates do this automatically when you scaffold a project.

## Project Structure

TypeScript projects follow a standard layout:

```text
my-project/
├── src/
│   ├── index.ts          # Library entry point (or main.ts for apps)
│   ├── serve.ts          # Server entry point (applications only)
│   └── api/              # Route handlers, modules, etc.
├── test/
│   └── *.test.ts         # Test files (*.test.ts or *.spec.ts)
├── .gen/                 # Generated files (auto-created by build)
├── tsconfig.json
└── package.json
```

## Quick Start

### Scaffold from a template

```bash
# Web application (React SSR)
putnami projects create my-app --template typescript-web

# API server (JSON, no React)
putnami projects create my-api --template typescript-server

# Library (reusable package)
putnami projects create my-lib --template typescript-library
```

### Run the development loop

```bash
# Serve the app with hot-reload
putnami serve my-app

# Run tests in watch mode
putnami test my-app --watch

# Lint and format
putnami lint my-app
```

### Build for production

```bash
# Full build: generate + transpile + types
putnami build my-app

# Build with compiled binary
putnami build my-app --compile
```

## Workspace Libraries

To use a workspace library in another project, add it as a `workspace:*` dependency:

```json
{
  "dependencies": {
    "@myorg/shared": "workspace:*"
  }
}
```

Then import it directly — Bun resolves workspace dependencies via symlinks at runtime.

## Next Steps

- **[Build](./02-build.md)** — 4-phase build pipeline, transpilation, type generation, and compilation
- **[Test](./03-test.md)** — Test runner, coverage, and debugging
- **[Lint](./04-lint.md)** — Biome formatting and linting with config resolution
- **[Serve](./05-serve.md)** — Development server with hot-reload
- **[Run](./06-run.md)** — Run a workload once and forward its exit code
- **[Package](./07-package.md)** — npm and Docker packaging for deployment
- **[Workspace Install](./08-workspace-install.md)** — Dependency installation
- **[Templates](./09-templates.md)** — Project scaffolding templates
