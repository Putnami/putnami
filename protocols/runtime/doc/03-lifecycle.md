# Lifecycle Phases

Putnami defines eight standard lifecycle phases that extensions implement as commands. Each phase has a well-defined purpose, dependency ordering, and expected output.

## Standard Phases

### generate

Code generation from schemas, protobuf definitions, OpenAPI specs, or other sources. Produces source files that subsequent phases consume.

- **Depends on**: none
- **Outputs**: generated source files
- **Example**: protobuf → Go/TS code, OpenAPI → client stubs

### build

Compile, transpile, and bundle source code into executable artifacts. Includes type checking for statically typed languages.

- **Depends on**: `generate`
- **Outputs**: compiled artifacts, type declarations, bundles
- **Example**: TypeScript transpilation, Go compilation, webpack bundling

### test

Execute test suites and report results as diagnostics. Produces coverage metrics and test count metrics.

- **Depends on**: `build`
- **Outputs**: test results (diagnostics), coverage reports (metrics, artifacts)
- **Example**: `bun test`, `go test`, `pytest`

### lint

Static analysis, formatting checks, and code style enforcement. Produces diagnostics for violations.

- **Depends on**: none
- **Outputs**: lint diagnostics
- **Example**: Biome, golangci-lint, Ruff

### format

Auto-format source code according to project conventions. Modifies files in place.

- **Depends on**: none
- **Outputs**: formatted source files (in place)
- **Example**: `biome format --write`, `gofmt`

### serve

Start the workload in development mode with file watching and hot reload. Only applicable to service and worker workloads.

- **Depends on**: `build`
- **Outputs**: running process (long-lived)
- **Example**: dev server with HMR, Go air reload

### publish

Publish packages to a registry. Typically gated by successful test and lint.

- **Depends on**: `build`, `test`, `lint`
- **Outputs**: published package reference
- **Example**: npm publish, Docker push, Go module tag

### package

Create distributable packages without publishing. Useful for CI artifacts and local testing.

- **Depends on**: `build`
- **Outputs**: package artifacts (tarballs, container images, binaries)
- **Example**: `docker build`, `npm pack`, cross-compiled binaries

## Phase Dependency Graph

```
generate ─────┐
              ├──→ build ──→ test ──→ publish
              │       │
              │       ├──→ serve
              │       │
              │       └──→ package
              │
lint ─────────┘                  ┘
format (independent)
```

Phases connected by arrows have explicit dependencies. `lint` and `format` are independent and can run in parallel with the build pipeline.

## Hooks

Extensions can inject custom logic at lifecycle boundaries using hooks. Each hook runs as a subprocess before or after its target phase.

| Hook | When |
|------|------|
| `preBuild` | Before the build phase starts |
| `postBuild` | After the build phase completes |
| `preTest` | Before the test phase starts |
| `postTest` | After the test phase completes |
| `preLint` | Before the lint phase starts |
| `postLint` | After the lint phase completes |
| `preServe` | Before the serve phase starts |
| `prePublish` | Before the publish phase starts |
| `postPublish` | After the publish phase completes |

Currently, only `preBuild` is implemented. Other hook points are reserved for future use.

## Pipeline Composition

Extensions implement phases as commands with multi-step pipelines. Each step in a pipeline references a task and declares dependencies on other steps:

```json
{
  "commands": {
    "build": {
      "run": [
        { "id": "generate", "task": "build-generate", "dependsOn": ["^generate"] },
        { "id": "transpile", "task": "build-transpile", "dependsOn": ["generate"] },
        { "id": "types", "task": "build-types", "dependsOn": ["generate", "transpile"] }
      ]
    }
  }
}
```

The `^` prefix references an upstream dependency's matching step. This enables cross-project dependency chains.

## Schema

The formal definition is in [`schemas/lifecycle.json`](../schemas/lifecycle.json).
