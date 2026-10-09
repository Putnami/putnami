# @putnami/go

Go extension for the Putnami workspace. Provides build, test, lint, and serve jobs for Go projects.

## Contract before code

`@putnami/go` is classified `stable` in the workspace `putnami.support.json`.
Before changing behaviour here, read what the change is allowed to move:

| Artifact | Holds |
| -------- | ----- |
| [`specs/go-project-toolchain.json`](specs/go-project-toolchain.json) | The observable requirements, each protected by named tests |
| [`doc/adr/0001`](doc/adr/0001-the-extension-owns-go-project-identity.md) | Why this extension — not the orchestrator — answers "what is a Go project", and why the replace closure is append-only |
| [`doc/adr/0002`](doc/adr/0002-generated-contracts-have-one-committer.md) | Why exactly one producer commits each generated contract, and why manifest paths are project-relative |
| [`doc/adr/0003`](doc/adr/0003-the-compile-matrix-is-declared-never-observed.md) | Why the compiled platform set never comes from the host |

Three invariants are easy to break by accident:

- **Probe purity.** Anything the probe reports must be derivable from the tree
  with no `go` invocation, no clock, and no absolute path. Its digest keys the
  workspace snapshot, so an impure fact is a cache-correctness bug.
- **One committer per artifact.** If describe will run, generate must not commit
  the sidecar. Two committers make the tracked contract depend on scheduling.
- **Declared matrix.** A new input that changes which platforms get compiled has
  to be a declared task parameter, or the cache will restore the wrong tree.

## Go Framework Modules

The Putnami Go framework lives in `go/framework/`. All modules use `go.putnami.dev/<name>` import paths and are stdlib-only (no external runtime dependencies, except `sql`, `grpc`, and `telemetry`).

| Module | Import | Purpose |
| ------ | ------ | ------- |
| `app` | `go.putnami.dev/app` | Plugin lifecycle, module composition, DI wiring, build-time describe mode |
| `http` | `go.putnami.dev/http` | Router, middleware, endpoint builder, SSE / WebSocket streaming, response helpers |
| `api` | `go.putnami.dev/api` | Transport-agnostic typed endpoint builder; feeds openapi / proto / gRPC discovery |
| `inject` | `go.putnami.dev/inject` | Hierarchical DI container (constructor-based + token-based) |
| `config` | `go.putnami.dev/config` | Multi-source config with YAML, env vars, struct tags |
| `logger` | `go.putnami.dev/logger` | Structured logging (JSON/console sinks, slog-compatible) |
| `events` | `go.putnami.dev/events` | Typed event topics with pluggable transports |
| `database` | `go.putnami.dev/database` | PostgreSQL (pgx), repository pattern, migrations, query builder |
| `errors` | `go.putnami.dev/errors` | Typed error codes with wrap/unwrap |
| `schema` | `go.putnami.dev/schema` | Struct-tag validation with type coercion |
| `security` | `go.putnami.dev/security` | Role/scope authorization middleware |
| `client` | `go.putnami.dev/client` | HTTP client with retry, circuit breaker |
| `cache` | `go.putnami.dev/cache` | Memory/disk/layered cache |
| `storage` | `go.putnami.dev/storage` | Object storage (memory, file, S3) |
| `telemetry` | `go.putnami.dev/telemetry` | OpenTelemetry tracing and metrics |
| `openapi` | `go.putnami.dev/openapi` | OpenAPI 3.0.3 spec generation (runtime + describe-mode) |
| `proto` | `go.putnami.dev/proto` | Protocol Buffer schema generation (runtime + describe-mode) |
| `grpc` | `go.putnami.dev/grpc` | gRPC server + Connect gateway |

## Minimal Go Server (Framework)

```go
package main

import (
    "go.putnami.dev/app"
    "go.putnami.dev/http"
    "go.putnami.dev/logger"
)

func main() {
    server := http.NewServerPlugin(http.ServerConfig{Port: 3000})
    server.Use(http.Recovery())
    server.Use(http.RequestID())
    server.Use(http.Logging(http.LoggerOptions{}))
    server.GET("/", func(ctx *http.Context) *http.Response {
        return http.JSON(map[string]string{"status": "ok"})
    })

    a := app.New("my-service")
    a.Use(server)
    a.Use(http.NewHealthPlugin())

    if err := a.ListenAndServe(); err != nil {
        logger.Default().Error("application failed", err)
    }
}
```

## Project Structure

### Simple layout

```text
my-project/
├── go.mod              # Module definition
├── main.go             # Entry point
├── main_test.go        # Tests
├── handler.go          # HTTP handlers (for larger projects)
└── putnami.json        # Project config
```

### Standard cmd/internal layout

```text
my-project/
├── cmd/
│   ├── serve/          # Server binary (auto-detected by serve job)
│   │   └── main.go
│   └── worker/         # Additional binary
│       └── main.go
├── internal/           # Private packages (Go toolchain enforced)
│   ├── handler/
│   └── service/
├── go.mod
└── putnami.json
```

Build auto-compiles all `cmd/*/main.go` targets. Serve auto-detects the right entrypoint (`serve` > `server` > `<project-name>` > `api`). Use `--entrypoint ./cmd/<name>` to override.

## Templates

| Template     | Description                                                                     |
| ------------ | ------------------------------------------------------------------------------- |
| `go-library` | Go module with an exported function and a test                                  |
| `go-server`  | HTTP server on the Go framework (`app`, `http`, `config`, `logger`, `platform`) |

Create a project: `putnami projects create my-api --template go-server`

Both templates live in `go/templates/` and are packaged by `@putnami/scaffold`,
not by this extension. Each declares its own feature and spec; the contract they
are held to is in
`tooling/scaffold/internal/packaging/committed_templates_test.go`.

## Extension Jobs

| Job | Command | Description |
| --- | ------- | ----------- |
| build | `./putnamiw build .` | Generate → describe → cross-compile |
| test | `./putnamiw test .` | Generate → describe → run tests with coverage |
| lint | `./putnamiw lint .` | golangci-lint + staticcheck + the skip guard |
| validate | `./putnamiw validate .` | Exported API of a stable project against its line's last tag |
| serve | `./putnamiw serve .` | Generate → describe → hot-reload development server |
| package | `./putnamiw package .` | Build pipeline + archives / Docker / Go module artifacts |

### Skip guard

Lint refuses a `t.Skip`, `t.Skipf` or `t.SkipNow` that runs unconditionally,
or whose reason or condition names CI or flakiness. Fix the test, or guard the
skip with a platform, dependency or `testing.Short()` check. A skip that stays
on purpose carries `//putnami:allow-skip <reason>` on its line or just above;
lint reports it as a warning. See [doc/lint.md](doc/lint.md#skip-guard).

Lint also fails on a relative link or an anchor in the project's `README.md`
files and `doc/` trees that does not resolve, including a path whose case
differs from the file on disk. Fix the link; do not turn the check off. See
[doc/lint.md](doc/lint.md#documentation-links).

### API compatibility

`validate` compares the exported API of a project that `putnami.support.json`
lists as stable, or of every project when the workspace has no catalog, with
the last tag of its version line. A removed or changed exported symbol fails
unless a commit since the tag that touches the project declares a breaking
change (`feat!:` or a `BREAKING CHANGE:` footer). Commit a deliberate break with
the marker, and put the marker in the pull request title too when the branch is
squash-merged. Do not work around the check. A project that ships a CLI
declares its committed command-surface document with the `command-surface`
option, and a removed command, flag, short alias or accepted value fails the
same way; an addition passes. See [doc/validate.md](doc/validate.md).

### Go build cache

Every `go` child runs against the machine-global cache root
(`PUTNAMI_GO_CACHE_DIR`, else `$PUTNAMI_HOME/cache/go`, else
`~/.putnami/cache/go`), mapped to `GOCACHE=<root>/build` and
`GOMODCACHE=<root>/mod`.

When the run is served by a cache provider, the extension also sets
`GOCACHEPROG` to `putnami-go gocacheprog` — a hidden entry point of this same
binary that backs Go's build cache with the provider's object cache: local
cache first, provider on a miss, asynchronous stores behind the compiler. Its
compiled objects live in `<root>/build` in Go's own format, so a `go` command
without the helper reuses them; its lookup records live in `<root>/prog`. It is
active only when the job environment carries the provider socket and the
`remote-build-cache` option is on (default), and any provider failure degrades
to the local cache. `putnami cache clean` / `cache gc` collect `<root>/prog`
alongside `<root>/build`. See `doc/build.md#remote-build-cache`.

### Workspace adapter (probe + sync)

The extension declares a `workspace` adapter, so it — not the orchestrator —
answers "what is a Go project?".

| Field | Value | Why |
| ----- | ----- | --- |
| `markers` | `go.mod` | A directory with a `go.mod` is a Go project |
| `inputs` | `go.mod`, `**/*.go`, `go.work` | ONE list gates BOTH halves of the adapter, so it is the union of what each half reads. The probe reads `go.mod` for identity and dependency edges, plus Go package clauses for library/application classification; the recursive Go-source glob is the classification witness whose matched-set digest notices creation or deletion of the first `package main`. The sync task also reads `go.work` (to walk the members whose closure it maintains, and to report a `go.mod` that `go.work` does not list). Dropping `go.work` would also stop `putnami install` noticing that the Go workspace membership moved, since core assembles its install-state fingerprint from the adapters' declared root-level inputs. `go.sum` and `go.work.sum` are deliberately absent: ordinary build and tidy flows rewrite them constantly, so hashing them would re-probe on every `go mod tidy` without changing a single reported fact |
| `excludes` | `testdata`, `vendor` | A `go.mod` under either is a fixture or a vendored copy, not a workspace project |
| `syncTask` | `workspace-sync-exec` | Owns the `go.mod` writes the CLI used to make |

**Probe** (`__putnami workspace-probe`, `cmd/putnami-go/workspace_probe.go`).
Pure and offline: it reads each candidate's `go.mod` and reports the module
path as the project's source identity plus its dependency edges as
**repo-relative paths** — resolved from `require` lines (direct *and*
`// indirect`) and from `replace` targets, both the module-path and the local
`./`/`../` forms. External modules contribute nothing; a project never depends
on itself. The answer is a function of the tree alone (no `go` invocation, no
clock, no absolute path) because its digest keys the workspace snapshot.

**Sync** (`workspace-sync`, `cmd/putnami-go/workspace_sync.go`). Maintains the
**workspace-replace closure**: module-mode `go mod tidy` ignores `go.work`, so
every workspace module in a member's *transitive* require graph needs that
member's own relative `replace`. Missing directives are appended, sorted, after
the existing content — existing bytes are never rewritten — and the changed
modules are then settled with `go mod tidy` (best effort). It runs
`workspace-once` and fails closed when it receives no resolved project
selection. It also warns when a selected project carries a `go.mod` that
`go.work` does not list, since the closure cannot see it.

The CLI still writes the same closure today, on purpose: this edit *bootstraps*
the extension that performs it — a syncTask runs on a prepared runtime, and in a
workspace that builds its own extensions from source, preparing that runtime
needs the closure a broken `go.mod` is missing. Running both is safe because the
codemod is append-only, deterministic and convergent, and the two
implementations are pinned byte-for-byte identical by a shared corpus.

It deliberately does **not** align a `module` line to the project name: a
project name (`@putnami/go`) and a module path (`go.putnami.dev/go/extension`)
are different identifiers, and writing the first over the second would break
every import.

### Generate phase (static AST)

A `generate` step runs before `describe` and `cross-compile`. It walks the
project's Go AST in-process and emits AST-only artifacts via a registry of
visitors. Today's only static visitor produces a path+method-only OpenAPI spec
to `schema/openapi.json` for projects that wire `*http.ServerPlugin` directly.
If describe mode later writes `schema/openapi.json` too, the runner merges the
two OpenAPI documents deterministically instead of replacing one API surface
with the other.

Adding a new AST-only generator is a sibling subpackage in
`go/extension/internal/codegen/<name>/` plus a blank import in
`cmd/putnami-go/main.go`. The visitor implements `codegen.Visitor`:

```go
import "go.putnami.dev/sdk/extension/codegen"

func init() { codegen.Register(&visitor{}) }

type visitor struct{}
func (v *visitor) Name() string { return "name" }
func (v *visitor) Visit(g *codegen.Generation) (*codegen.Result, error) {
    // walk g.Files, return Result{ SchemaFiles, Exports, Assets }
}
```

### Describe phase (build-time full-fidelity)

A `describe` step runs after `generate` and before `cross-compile` (or
`compile` for `serve`). It compiles a host binary, runs it with
`PUTNAMI_DESCRIBE=all PUTNAMI_DESCRIBE_OUT=<.gen>`, and captures the artifacts
the runtime plugins emit via `app.Describer` — full type fidelity with no need
to parse Go source.

Describe entrypoint selection is explicit for multi-binary projects:

- Root `main` package: used automatically.
- Single `cmd/<name>/main.go`: used automatically.
- Multiple `cmd/*/main.go`: fails with a diagnostic. Set
  `options["@putnami/go"].describe.entrypoint` in `putnami.json` (for example
  `./cmd/api`) or pass `--describe-entrypoint`.
- `options["@putnami/go"].entrypoint` remains a fallback when a project already
  has one canonical application binary.

Plugins opt in by implementing `Describer`; today `go.putnami.dev/openapi`
emits `schema/openapi.json` (+ `.gz`) and `go.putnami.dev/proto` emits
`schema/api.proto`. Streaming RPCs get the proper `stream` modifiers in proto
because the runtime generator runs.

Library projects (no main package) and CLI binaries (no `go.putnami.dev/app`
dependency) are skipped automatically.

### Schema artifacts

Generated schemas live in two places:

| Path | Source |
| ---- | ------ |
| `<project>/.gen/schema/<name>.<ext>` | Always written; build-only |
| `<project>/schema/<name>.<ext>` | Committed; suppressed by `options.generate.schema=false` in `putnami.json` |

`schema/openapi.json` is canonicalized before write: JSON object keys are
stable, parameters and required lists are sorted, and duplicate generate /
describe producers are merged by path and method. Non-OpenAPI artifacts with
multiple producers fail instead of overwriting silently.

The Putnami orchestrator's project-dependency ordering ensures consumer projects
(e.g. a TS service) see the committed `schema/` files before their own build
runs — supporting cross-language codegen in a monorepo without runtime plumbing.

## Reference Sample

See `go/samples/task-api/` for a complete example using app, http, inject, config, logger, and events together.
