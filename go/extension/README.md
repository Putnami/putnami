# @putnami/go

Go build tooling for Putnami workspaces — build, test, lint, serve, run, and package Go projects alongside TypeScript and Python.

Go task inputs include files named by `//go:embed` directives. An embedded
asset-only edit invalidates the relevant build or test cache key; build tasks
exclude directives in `_test.go` sources, while test tasks include them.
Directives in potentially buildable platform or tag variants are included
independently of the current host, so every such target must resolve.

## Features

- **Build** — compile Go binaries with cross-compilation (5 platforms)
- **Test** — run tests with coverage, metrics, and per-file breakdown
- **Lint** — golangci-lint + staticcheck, auto-installed on first use
- **Validate** — a stable project's exported API cannot break without a breaking-change marker
- **Serve** — hot-reload in development, pre-built binary in production
- **Run** — run a workload once for the host and forward its exit code
- **Package** — create release archives, Docker images, and Go module distributions
- **Zero-dependency** — auto-manages the Go toolchain; bash, jq, and curl are the only prerequisites

## Installation

`@putnami/go` is included in every Putnami workspace by default. To add it manually:

```bash
putnami deps add @putnami/go
```

## Quick Start

```bash
# Scaffold a server (recommended)
putnami projects create api-gateway --template go-server

# Or scaffold a library
putnami projects create go-lib --template go-library

# Build, test, lint
putnami build api-gateway
putnami test api-gateway
putnami lint api-gateway

# Develop with hot-reload
putnami serve api-gateway
```

## Jobs

| Job | Command | Description | Docs |
|-----|---------|-------------|------|
| Build | `putnami build` | Compile with cross-compilation, automatic dependency sync | [doc/build.md](./doc/build.md) |
| Test | `putnami test` | Run tests with coverage profile and per-file breakdown | [doc/test.md](./doc/test.md) |
| Lint | `putnami lint` | golangci-lint + staticcheck, auto-fix support | [doc/lint.md](./doc/lint.md) |
| Validate | `putnami validate` | Fail an incompatible API change of a stable project without a breaking-change marker | [doc/validate.md](./doc/validate.md) |
| Serve | `putnami serve` | Hot-reload dev server or pre-built production binary | [doc/serve.md](./doc/serve.md) |
| Run | `putnami run` | Run a workload once for the host and forward its exit code | [doc/run.md](./doc/run.md) |
| Package | `putnami package` | Create archives, Docker images, Go module distributions | [doc/package.md](./doc/package.md) |

## Documentation

- **[Getting Started](doc/getting-started.md)** — project setup, templates, manual setup, toolchain management
- **[Build](doc/build.md)** — cross-compilation, dependency sync, linker flags, `--install`
- **[Test](doc/test.md)** — coverage modes, benchmarks, flaky test detection
- **[Lint](doc/lint.md)** — golangci-lint config resolution, staticcheck, auto-fix
- **[Validate](doc/validate.md)** — the API check that holds the breaking-change marker for stable projects
- **[Serve](doc/serve.md)** — dev vs production modes, file watching, environment variables
- **[Run](doc/run.md)** — one-shot workload runner with exit-code forwarding
- **[Package](doc/package.md)** — archives, Docker, Go module channels
- **[Exact dependency documentation](doc/dependency-documentation.md)** — version-resolved, offline MCP documentation reads

## Toolchain Management

Go is auto-managed. If the `go` binary in PATH is not the release `putnami.lock.json` pins, the extension downloads and verifies the pinned release, and installs it once for the machine under `~/.putnami/toolchains/go/go-<version>/`. Every workspace that pins the release runs that install. Without a pin, the workspace's Go version is a minimum, read from:

1. `go.work` directive at workspace root
2. `go.mod` in the project directory
3. Latest stable from `https://go.dev/VERSION`
4. Fallback: `1.23.0`

Lint tools are pinned in [`tools/versions.json`](./tools/versions.json), which is
the machine-readable contract for CI images and setup scripts. `putnami install`
warms one copy per machine under
`$PUTNAMI_HOME/tools/go/<tool>/<version>/go<major.minor>/<goos>-<goarch>/`
(`PUTNAMI_HOME` defaults to `~/.putnami`), restoring the prebuilt binary the
extension archive ships at `compiled/tools/<tool>` instead of compiling it. Lint
reads that same path, and validates any PATH binary against the pin before
reusing it. See
[doc/getting-started.md](./doc/getting-started.md#pinned-lint-tools) for the
resolution order and for fetching another platform's binary.

## Extension Configuration

The extension activates automatically on any project with `go.mod` or `*.go` files. No extension configuration is required; a project only needs the `putnami.json` every Putnami project has.

To enable package channels:

```json
{
  "name": "@myworkspace/api-gateway",
  "options": {
    "package": {
      "archives": true,
      "docker": true
    }
  }
}
```

## Support and contract

`@putnami/go` is a public, documented, maintained package classified `stable` in
the workspace [support catalog](../../putnami.support.json). The
[go-project-toolchain specification](specs/go-project-toolchain.json) states the
observable promise, and three accepted decisions explain the durable choices
behind it:

- [the extension owns Go project identity and the workspace replace closure](doc/adr/0001-the-extension-owns-go-project-identity.md);
- [generated contracts have exactly one committer, and the manifest relocates](doc/adr/0002-generated-contracts-have-one-committer.md);
- [the compile matrix is declared, never observed](doc/adr/0003-the-compile-matrix-is-declared-never-observed.md).

The contract covers the managed toolchain and pinned lint tools, the pure
workspace probe and the append-only replace closure, restoration of module files
a build mutates, a declaration-driven compile matrix, single-committer schema
generation with checkout-relocatable manifests, host-identity scrubbing and
per-project failure attribution in tests, batch/solo equivalence in lint, and an
additive package-channel index. Registry upload belongs to `publish`, and client
generation for other languages belongs to `@putnami/clientgen`. No default or
cross-language parity claim is made. Before v1.0.0, minor `0.x` releases may
contain documented breaking changes — see [RELEASE.md](../../RELEASE.md).

## License

[FSL-1.1-MIT](../../LICENSE.md)


Image-project packaging declares `build` as a conditional session prerequisite.
This materializes generated inputs from other installed extensions before OCI
assembly, even when the image is selected only through another package's
`^image` dependency. Image projects still skip Go compiler steps. Workload and
archive packaging keep their existing pipeline, and a static image with no
build producer has no additional executable work.

Archive packaging sorts entries and normalizes timestamps and host ownership.
Independent staging of the same files produces identical gzip/tar bytes, while
file contents, executable modes and symbolic links remain part of the artifact.
This permits exact immutable publication readback on a retry.
