# @putnami/typescript

TypeScript extension for Putnami — provides build, test, lint, serve, run, and package commands using Bun and Biome.

## Features

- **Build** — 4-phase pipeline (generate, transpile, types, compile) with independent caching
- **Test** — Bun test runner with JUnit reports and LCOV coverage
- **Lint** — Biome formatting and linting with zero-config defaults
- **Serve** — Development server with hot-reload, debugger, and structured log forwarding
- **Run** — One-shot workload runner that forwards the child process exit code
- **Package** — npm tarball and Docker image creation from build output
- **Workspace Install** — Workspace-wide `bun install`; on a hosted run, `workspace-fetch` first downloads the locked packages without running their code
- **Templates** — Scaffold libraries, API servers, and React web apps

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

## Quick Start

```bash
# Create a new project
putnami projects create my-app --template typescript-web

# Development loop
putnami serve my-app
putnami test my-app --watch
putnami lint my-app

# Build for production
putnami build my-app
putnami build my-app --compile    # With standalone binary
```

## Commands

| Command | Purpose | Key Flags |
|---------|---------|-----------|
| `putnami build` | Compile TypeScript to JS, types, and executables | `--compile`, `--target`, `--bundle`, `--release` |
| `putnami test` | Run tests with Bun's test runner | `--watch`, `--coverage`, `--coverage-threshold`, `--bail`, `--timeout`, `--timeout-budget` |
| `putnami lint` | Format and lint with Biome | `--fix`, `--diagnostic-level` |
| `putnami serve` | Start dev server with hot-reload | `--port`, `--debug`, `--inspect` |
| `putnami run` | Run a workload once and forward its exit code | `--entrypoint`, `--port`, `--args` |
| `putnami package` | Create npm/Docker packages | `--npm`, `--docker`, `--stable` |
| `putnami version` | Report the project's version metadata | — |

## Architecture

This extension is implemented as a Go binary (`putnami-ts`) that provides all
subcommands. Its v3 extension manifest declares that binary as
`{extensionRuntime}`: local source prepares it once into the digest-keyed
artifact store, while platform archives ship the same `compiled/putnami-ts`
path directly.

### Build Pipeline

```text
generate ──→ transpile ──→ types
         └──→ compile
```

### Lint Pipeline

```text
format ──→ check
```

## Documentation

- **[Getting Started](doc/01-getting-started.md)** — Setup, project structure, and quick start
- **[Build](doc/02-build.md)** — 4-phase build pipeline, flags, output layout
- **[Test](doc/03-test.md)** — Test runner, coverage, JUnit output
- **[Lint](doc/04-lint.md)** — Biome formatting, linting, config resolution
- **[Serve](doc/05-serve.md)** — Dev server, port management, log forwarding
- **[Run](doc/06-run.md)** — One-shot workload runner with exit-code forwarding
- **[Package](doc/07-package.md)** — npm and Docker packaging
- **[Workspace Install](doc/08-workspace-install.md)** — Dependency installation
- **[Templates](doc/09-templates.md)** — Project scaffolding templates
- **[Exact dependency documentation](doc/10-dependency-documentation.md)** — per-project, version-resolved MCP documentation reads

## Support and contract

`@putnami/typescript` is a public, documented, maintained package classified
`stable` in the workspace [support catalog](../../putnami.support.json). The
[typescript-project-toolchain specification](specs/typescript-project-toolchain.json)
states the observable promise, and three accepted decisions explain the durable
choices behind it:

- [a batched phase must equal the solo runs it replaces](doc/adr/0001-batched-phases-must-equal-solo-runs.md);
- [one workspace catalog owns shared dependency versions](doc/adr/0002-one-workspace-catalog-owns-shared-versions.md);
- [a package must load before it ships](doc/adr/0003-a-package-must-load-before-it-ships.md).

The contract covers independently cached build phases with per-project output
isolation, batch/solo equivalence and per-project failure isolation, catalog
seeding that never overwrites an existing entry or scrambles the root manifest's
key order, serve entrypoint resolution and process-group supervision, a
loadability gate and contract ratchet before any package ships, honest asset
declarations, Biome coverage of every writable project file, deterministic hook
ordering, and structured test/coverage reports. Registry upload belongs to
`publish`. Application framework behaviour belongs to the `@putnami/*` packages
this extension builds. No default or cross-language parity claim is made. Before
v1.0.0, minor `0.x` releases may contain documented breaking changes — see
[RELEASE.md](../../RELEASE.md).

## License

[FSL-1.1-MIT](../../LICENSE.md)
