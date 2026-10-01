# Go Extension

The `@putnami/go` extension provides the Go project lifecycle inside a Putnami workspace: toolchain resolution, build, test, lint, serve, packaging, Docker output, and cache-aware dependency handling.

Go projects stay Go-native, but the workflow stays workspace-native.

## Extension phases

| Phase | Start here | What it explains |
|-------|------------|------------------|
| Setup | [Toolchain & detection](/docs/frameworks/go/extension/toolchain-and-detection) | How Go projects are detected and how the Go toolchain is resolved |
| Feedback loop | [Build, test & lint](/docs/frameworks/go/extension/build-test-lint) | Compile, test, coverage, golangci-lint, and staticcheck |
| Local runtime | [Serve & watch](/docs/frameworks/go/extension/serve-and-watch) | Entrypoint detection, hot reload, and `PORT` handling |
| Release artifacts | [Package & Docker](/docs/frameworks/go/extension/package-and-docker) | Archives, Docker images, and release packaging |
| Repeatability | [Caching & dependencies](/docs/frameworks/go/extension/caching-and-dependencies) | Cache inputs, `go.mod` sync, and workspace dependency tracking |

## Enable the extension

`@putnami/go` is included by default in new Putnami workspaces. To add it to an existing workspace:

```bash
putnami deps add @putnami/go
putnami deps install
```

## External tools

- Go, resolved automatically by the extension
- [golangci-lint](https://golangci-lint.run)
- [staticcheck](https://staticcheck.dev)

The extension can download the required Go toolchain and lint tools on first use.

## Daily loop

```bash
putnami test api
putnami serve api
putnami lint,test,build --impacted
```
