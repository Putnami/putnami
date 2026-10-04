# Build

**Command:** `putnami build [project]`

Compiles Go packages and binaries **for the host platform**, with cross-compilation available as an explicit opt-in. In a combined `lint,test,build` invocation, an ordinary library compile is omitted only when test and lint provide equivalent host compile evidence, including a matching race setting. Automatically keeps `go.mod` and the project's `putnami.json` in sync so workspace dependencies are always tracked.

## Overview

- Runs `go mod tidy` and syncs workspace dependencies before compiling
- Generates schema artifacts before compiling
- Runs build-time describe mode for runtime plugin artifacts such as OpenAPI
- Compiles the describe entrypoint with the compile step's own configuration
  (`mod`/`readonly`, `cgo`, `buildvcs`, `tags`, `gcflags`, `asmflags`, `race`,
  `trimpath`, `installsuffix`, `p`, `buildmode`), so the two steps build one program, compile
  reuses Go's build cache, and compiling is a link rather than a second
  compilation of every package above `net`. Every one of those parameters is a
  declared cache-key input of both steps, except where a task pins the value in
  its own invocation — the `package` cross-compile pins `-trimpath` and CGO off
- Detects binaries automatically by scanning `cmd/*/main.go`; falls back to the root package
- Compiles for the **host platform only** by default; cross-compilation is opted
  into with `--target` / `platforms`, and the full release matrix belongs to
  [`package`](./package.md)
- Compile-checks libraries (`go build ./...`) instead of linking a binary they
  cannot produce; a top-level `lint,test,build` validation run omits only this
  redundant host-default step
- Writes a `VERSION` file from the workspace version alongside the output binary
- Emits a `binary-size` metric after each successful compile

**Activation:** Any project containing `go.mod` or `*.go` files.

## Project classification

The workspace probe classifies every Go project as an **application** or a
**library**, and the rule is the only one Go gives us:

> A module that contains a `package main` can produce an executable and is an
> `application`. A module that contains none can only be imported and is a
> `library`.

The scan starts at the module's own `go.mod` directory and ignores every
`package main` the module's own build never links:

| Ignored | Why |
| --- | --- |
| a subdirectory with its own `go.mod` | that binary belongs to the nested module |
| `vendor/`, `testdata/`, `_`- and `.`-prefixed directories | invisible to the build |
| `*_test.go` | a test binary is the go tool's, not the module's |
| a file no tag assignment can satisfy (`//go:build ignore`) | never linked into anything |

Build constraints are evaluated **without** reading `GOOS`/`GOARCH`. A command
behind `//go:build windows` is still a command, and the classification of one
tree must not change with the host that probed it — the probe's answer digests
into the workspace snapshot that keys the cache.

An authored `putnami.json` `"type"` always outranks the derived value. Use it
for the cases the rule cannot see, such as a workload whose entrypoint is built
outside the module.

What the classification changes:

- library projects plan no `serve` and no `run` — the verbs that require a
  runnable entrypoint;
- library projects are not deployable workloads, so they are excluded from infra
  aggregation and from `putnami infra plan`;
- standalone ordinary `build` compiles a library with `go build ./...` and emits
  no binary, instead of linking the root package; a combined validation run can
  use its same-invocation test and lint evidence instead (see
  [Build contract by project nature](#build-contract-by-project-nature));
- `test`, `lint`, `package`, and `publish` are unaffected.

## Build contract by project nature

`build` answers one question: **does this project still compile?** What that
costs is decided by two things, and only two:

- **Nature** — what the project *is*. It sets the default evidence.
- **Intent** — which command you ran. It sets the platform coverage that
  evidence must have.

Nature supplies defaults; **intent is authoritative**. Ordinary validation
(`build`) never compiles for a platform you did not ask for, because no step of
`build` consumes the binaries and Go cross-compiles share no `GOCACHE` objects
across `GOOS`/`GOARCH` — five platforms is five full compiles, not one compile
and four cheap variations.

Cross-compile and publish-path `go` invocations retry a bounded number of times
when they fail with an ENOENT referencing the machine-global `GOCACHE` root:
that tree is deletable concurrently (the cache policy sweeps `<root>/build`
while other jobs point at it), so the class is transient by construction. A
persistent failure still surfaces after the bounded attempts.

| Nature | How it is recognised | Standalone ordinary `build` compiles | Emits a binary | Full platform matrix |
|---|---|---|---|---|
| **Library** | no `package main` in the module | `go build ./...`, host platform | no — the compile *is* the evidence | `--target` / `platforms` only |
| **Local runnable** | `package main`, run with `serve` / `run` | entrypoints, host platform | yes, into the command output dir | `--target` / `platforms` only |
| **Container workload** | `package main` + deployable workload (infra requirements) | entrypoints, host platform | yes | `putnami package --docker` |
| **Distributed binary** | `package main` published as an archive | entrypoints, host platform | yes | `putnami package --archives` (all five archive platforms) |
| **Sample** | ordinary project under a samples tree | same as its own nature | per its nature | not requested by default |
| **Template** | `"type": "template"`; sources are `*.template` files | nothing — no extension activates, so no job is planned | no | n/a |

### Combined validation evidence

When the same Go provider/project scope has effective top-level `lint`, `test`,
and `build` commands, the planner omits `build~compile` for a library whose
build parameters describe the ordinary host default and whose resolved `race`
setting matches test. `go test ./...` compiles packages even when they contain
no `_test.go` files, while golangci-lint/staticcheck provide an independent
type/compile check. The omitted `go build ./...` would therefore add no evidence
to that invocation.

The optimization is deliberately narrow:

- `putnami build` by itself always retains the library compile check;
- applications always retain `build~compile`, because it emits their binary;
- a build synthesized as another command's dependency is not treated as an
  explicit top-level validation request;
- any build-only compile-shaping request retains the library compile, including
  target or platform selection, module mode, build tags,
  compiler/assembler/linker flags, trimpath/buildmode/install-suffix settings,
  CGO, parallelism, and version injection;
- race retains the plain compile whenever build and test resolve different
  values—including test-only `options.test.race` and
  `options["@putnami/go:test"].race`; matching values compile the same Go source
  set and may still delegate;
- generation and infra work remain in the DAG, while describe keeps its existing
  source-file activation; this optimization removes only the redundant compile
  node and its edge.

The command set is represented as an order-independent set, so
`lint,test,build` and `build,lint,test` produce the same plan. If the planner
cannot resolve the invocation or project facts, the condition fails safe by
keeping the compile.

Samples and templates have no separate build path on purpose. A sample is an
ordinary project and is validated exactly like the nature it has. A template is a
workspace project, but it holds `*.template` sources rather than `.go` files, so
nothing activates on it and `putnami build` reports *no jobs matched* — its
validation is that `putnami new` can materialise it, which is the scaffold
extension's contract, not this one's. Inventing a build nature for either would
add a classification axis with no behaviour behind it.

The host platform is `runtime.GOOS/runtime.GOARCH` of the machine running the
build.

### Platform-sensitive projects

Build tags, CGO, hand-written assembly, and platform-specific source files are
**explicit configuration**, never inferred. `build` does not scan for
`//go:build windows` or `.s` files and quietly widen the matrix: a heuristic that
adds platforms is a heuristic that adds minutes, and it does so on exactly the
projects whose authors are best placed to say what they need. Declare the
platforms you actually require:

```json
{
  "options": {
    "@putnami/go": {
      "platforms": ["linux/amd64", "darwin/arm64"]
    }
  }
}
```

A platform request widens the platform **set**; it does not change the **kind**
of evidence a project owes. A library asked for two platforms is compile-checked
twice and still emits no binary — which is the point, since a library is the
usual home of `//go:build` variants.

### Migration from the implicit four-platform default

`build` used to cross-compile every project for `linux/amd64`, `linux/arm64`,
`darwin/amd64`, and `darwin/arm64` unconditionally. If you relied on that, pick
the replacement that matches *why* you relied on it:

| You relied on `build` for… | Use instead |
|---|---|
| release artifacts for all platforms | `putnami package .` — the release matrix is unchanged and still covers all four |
| one specific non-host platform | `putnami build . --target linux/amd64` |
| a fixed set of platforms, every time | `options["@putnami/go"].platforms` in the project's `putnami.json` |
| proof that the code compiles | nothing — the host build already proves it |

`--target` takes a single value and wins over `platforms`; it is the narrower,
per-invocation request. Both accept the same vocabulary, resolved by the same
code, so no word means one thing to one flag and something else to the other:

| Value | Resolves to |
|---|---|
| `linux/amd64` | that one platform |
| `linux` | every archive platform for that OS (`linux/amd64`, `linux/arm64`) |
| `freebsd` | an OS outside the archive matrix keeps the host architecture |
| `host` | the machine running the build |
| `all` | the five archive platforms |

`platforms` additionally accepts a list — a JSON array in `putnami.json`, or a
comma-separated string on the command line. Duplicates are dropped and the
requested order is preserved, so one spec always resolves to one set. A value
that is neither a platform string nor an array of them fails the build with a
diagnostic naming the offending value, rather than being passed to the toolchain
as a `GOOS`.

### Why the platform set is in the cache key

Both flags are **plan-time parameters**: the orchestrator resolves them before
the task runs and folds them into the task's cache key, so a compile that
covered one platform set is never served as the answer for another. The set is
passed *into* the build task rather than read by it, because a value the task
discovered for itself would change the bytes it produced without changing where
those bytes were stored.

The **default** set is the host, and the host is a property of the machine, not
of the parameters — so `build-compile` also declares the `hostPlatform` runtime
input, which puts `GOOS/GOARCH` in its key. Nothing else in the key does this:
the toolchain component is `runtime.Version()`, the same `go1.x` string on every
operating system, and the extension-implementation digest applies only to
workspace-local extensions, so a workspace consuming a published `@putnami/go`
would have no machine-bearing component at all. Since cache entries are shared
between developer machines and CI on purpose, without that declaration a darwin
laptop's entry could be restored on a linux runner: the app's `bin/<name>` would
be a Mach-O binary, and a library's compile-check verdict would be reused across
operating systems even though `//go:build linux` files are only ever compiled on
linux — so linux-only compile errors would pass CI.

> The same gap is **pre-existing** on `test-exec`, whose key likewise has no host
> component. It is deliberately out of scope here and left to its own change.

### Why test files are not in the build cache key

`build-generate`, `build-describe` and `build-compile` declare their sources as
`["**/*.go", "!**/*_test.go"]`. `go build` never compiles a test file, and
describe's host binary is a `go build` of the project's main package, so no byte
these tasks emit is a function of one — the same rule the classification scan
above applies.

The exclusion matters most on `build-describe`, which carries the `^describe`
edge: every dependent reaches its dependencies through it, and a dependent's
`compile` and `test-exec` sit behind its own `describe`. A test file in
describe's key therefore moves the identity of a project's whole dependent
closure. Measured on this repository with a warm cache, one blank line appended
to a `go/framework/http` test file re-ran 12 cacheable tasks across four
projects while the exclusion was missing, and one — the owning project's own
`test` — with it.

`test-exec` declares `**/*_test.go`, because it runs those files: excluding them
there would serve a stored verdict for a test the run never executed.

## Execution Flow

1. **Tidy** (unless `--skip-deps`):
   - Scans Go imports for matches against workspace modules
   - Adds `replace` directives to `go.mod` for workspace dependencies
   - Runs `go mod tidy`, unless the environment disables module downloads
     (`GOPROXY=off`). Tidy resolves the full module graph — every imported
     package's test dependencies, and a fresh version query for each
     requirement it has to add — which is wider than the build list `putnami
     install` warms, so an offline run reports the phase as a no-op, logs why,
     and leaves `go.mod`/`go.sum` as it found them. `GOPROXY` is a cache-key
     input of `build-tidy`, so that entry is reused by the next offline build
     of the same tree and never answers a run that can resolve modules. A tidy
     that ran and failed is still an uncacheable skip, so a transient network
     failure is retried rather than recorded.
   - Reports a `warning` diagnostic when a successful tidy added a workspace
     module to `go.mod` (test-only imports count: tidy requires what
     `_test.go` files import). The release-set probe derives a published
     member's dependencies from the *committed* `go.mod`, so a requirement
     that only exists after tidy is absent from the release-set plan and
     `package~go` refuses the module. The remedy is to commit `go.mod` and run
     `putnami projects sync` so every dependent's replace closure follows.
   - Syncs `go.mod` dependencies back to `putnami.json`
2. **Generate**:
   - Parses non-test Go sources
   - Runs static codegen visitors
   - Writes generated schemas under `.gen/schema/`
   - Copies committed schemas to `schema/` unless `options.generate.schema=false`
3. **Describe** (only for projects that import `go.putnami.dev/app`):
   - Compiles the configured describe entrypoint for the host platform, with the
     compile step's build configuration (everything but `-ldflags`, which reaches
     the link action alone)
   - Runs it with `PUTNAMI_DESCRIBE=all`
   - Captures artifacts emitted by `app.Describer` plugins
   - Merges duplicate `schema/openapi.json` producers deterministically
   - For projects whose `go.mod` does not require `go.putnami.dev/app`, this
     step is pruned from the plan entirely (via a step `activation` gate)
     instead of being scheduled and skipped. Its dependents reconnect to
     `generate`, so the plan shape is unchanged for framework apps.
4. **Compile**:
   - Resolves the platform set from the plan-time `--target` / `platforms`
     parameters; with neither set, the set is the host alone
   - **Libraries** (no `package main`): runs `go build ./...` once per resolved
     platform and emits no binary — except that the ordinary host-default step
     is pruned when the same top-level invocation also runs `test` and `lint`
   - **Applications**: auto-detects entry points (`cmd/*/main.go`, then `.`),
     applies the `--entrypoint` override if provided, and executes `go build`
     with the configured flags
   - Copies binary to `--install` path if specified
   - Writes `VERSION` file next to the output binary
   - An explicit platform request additionally applies the distribution-flavoured
     defaults (`-trimpath`, CGO off unless `--cgo` says otherwise) and writes the
     platform-partitioned `bin/<platform-suffix>/` tree
5. **Infra** (workloads only):
   - Merges the committed `infra/requirements.json` of every project in the
     workload's dependency closure (the closure arrives on the job context)
   - Applies `<workload>/infra/overrides.json`
   - Resolves the runtime block: an authored `<workload>/infra/runtime.json`
     wins and its stale defaults sidecar is removed; otherwise framework
     defaults are synthesized and written to `<workload>/.gen/infra/runtime.json`
   - Writes `<workload>/.gen/requirements.json` atomically
   - Findings (a malformed contribution, a merge conflict, an unused override)
     are reported as task warnings and never fail the build
   - Skipped for libraries (see [Project classification](#project-classification)),
     and never cached — its inputs are other projects' committed manifests, which
     no per-project cache key covers
   - The `package` command's `deployment` step writes the same aggregate as
     the workload's deployment declaration (see
     [Package](package.md#deployment-channel))

## Usage

### Basic

```bash
putnami build .
```

Compiles for the host platform. This is the default for every project nature.
For a library, this standalone form always plans `build~compile`; only the
combined top-level validation trio can reuse test+lint compile evidence.

### Cross-compilation (opt-in)

```bash
# Linux x86-64
putnami build . --target linux/amd64

# macOS Apple Silicon
putnami build . --target darwin/arm64

# Windows x86-64
putnami build . --target windows/amd64

# An explicit set, in one invocation
putnami build . --platforms linux/amd64,darwin/arm64

# The five archive platforms, without going through `package`
putnami build . --platforms all
```

Or per project, so every `build` of it covers the same set:

```json
{
  "options": {
    "@putnami/go": {
      "platforms": ["linux/amd64", "linux/arm64"]
    }
  }
}
```

CGO is automatically disabled when cross-compiling. Enable it explicitly with `--cgo`.

### Custom output path

```bash
# Write binary to a specific path
putnami build . --output_path ./bin/api-gateway
```

### Production binary

```bash
# Strip debug info and symbol table (smaller binary)
putnami build . --ldflags "-s -w"

# Inject version at link time
putnami build . --ldflags "-s -w -X main.version=1.2.3"

# Reproducible build (remove local file system paths)
putnami build . --trimpath --ldflags "-s -w"
```

### Debugging

```bash
# Disable optimisations and inlining (for use with dlv)
putnami build . --gcflags "-N -l"
```

### Build tags

```bash
putnami build . --tags "integration,mock"
```

### Fast rebuild (skip dependency sync)

```bash
putnami build . --skip-deps
```

### Install binary to a known path

```bash
# Copy the compiled binary to bin/api-gateway (relative to workspace root)
putnami build . --install bin/api-gateway
```

## OpenAPI generation

Go projects can produce `schema/openapi.json` from two sources:

- `build-generate`: a static AST visitor records direct `go.putnami.dev/http` server routes.
- `build-describe`: runtime `app.Describer` plugins such as `go.putnami.dev/openapi` write full-fidelity specs from configured plugins.

When both write `schema/openapi.json`, the describe runner merges the documents
by path and method so one HTTP surface does not replace another. JSON output is
canonicalized before writing: object keys are stable, parameters are sorted by
location/name, and schema `required` lists are sorted. Framework OpenAPI plugins
emit that canonical form directly, so publication preserves the exact bytes and
contract hash used by in-process client generation when publication only
reapplies the object-key and trailing-newline format. A merge or normalization
of provider-authored values is a semantic contract change. If a non-OpenAPI schema
has multiple producers with different content, the build fails with a diagnostic
instead of overwriting silently.

Canonicalization normalizes structure, never the values you declare:

- Everything under `default`, `example`, `examples`, `enum`, `const` and any
  `x-` extension is copied verbatim. A default of `["validate", "render"]` keeps
  that order, and a business field named `required` or `parameters` is not
  mistaken for the OpenAPI keyword of the same name.
- `servers`, `security` and `tags` keep the order you declared and are only
  deduplicated. The first server is the default base URL and a `security` array
  is an ordered list of alternatives, so sorting them would rewrite the
  contract.
- When both producers describe the same value, the describe document wins whole
  rather than field by field: a static stub's guessed example cannot leave
  fields behind inside the example the provider declared.

Multi-binary projects must make the describe binary unambiguous. If a project
has more than one `cmd/*/main.go` and no root `main` package, set:

```json
{
  "options": {
    "@putnami/go": {
      "describe": {
        "entrypoint": "./cmd/api"
      }
    }
  }
}
```

`options["@putnami/go"].entrypoint` is also used as a fallback when it already
names the canonical application binary. For one-off runs, pass
`--describe-entrypoint ./cmd/api`.

## HTTP route inventory

Framework HTTP servers emit `schema/http-routes.json` during describe mode. The
artifact uses `putnami.http-routes.v1` and is built from the configured runtime
route registry, so typed API routes and direct `http.ServerPlugin` routes are
included even when OpenAPI is disabled. Named parameters are preserved as
single-segment templates, methods are canonicalized, and the output carries a
stable digest.

Catch-alls, wildcards, and other patterns that cannot be represented safely by
v1 fail the build with an `http_routes.unsupported_pattern` diagnostic instead
of widening the public edge route.

## Dependency Management

The build job maintains a two-way sync between `go.mod` and the project's `putnami.json`:

### Automatic discovery (Go imports → putnami.json)

When your Go source imports a module that belongs to another workspace project, the build:

1. Adds a `replace` directive to `go.mod` pointing at the local path
2. Runs `go mod tidy` to materialise the `require` entry
3. Adds the project to `dependencies` in `putnami.json`

**Example — source imports `go.putnami.dev/http`:**

```go
import "go.putnami.dev/http"
```

After build:

```
# go.mod
replace go.putnami.dev/http => ../../go/framework/http

require go.putnami.dev/http v0.0.0-00010101000000-000000000000
```

```json
// putnami.json
{
  "dependencies": ["go.putnami.dev/http"]
}
```

### Manual declaration (putnami.json → go.mod)

You can also declare dependencies manually in `putnami.json`. For each workspace dependency with a `go.mod`, the build adds the `replace` directive automatically on the next run.

## Output Location

Every step of one command shares a single output directory,
`.putnami/out/{project}/{command}/`, and each step owns an exact subpath of it.
Unless `--output_path` is specified, the binaries are written to:

```
# host build (the default)
.putnami/out/{project}/build/bin/{binary-name}

# explicit --target / platforms request, and every `package` run
.putnami/out/{project}/build/bin/{platform-suffix}/{binary-name}
```

The host build is unpartitioned because there is exactly one platform to name,
and naming it would make the path depend on the machine that produced it. An
explicit platform request keeps the partitioned tree the package channels index
by. Libraries write no `bin/` at all.

The binary name is derived from the project name. A `VERSION` file is written to
the command output directory in every case, including for libraries: it records
the workspace version the output tree was built at, which is true whether or not
the tree also holds binaries.

## Declared Outputs

The manifest states each task's filesystem footprint (the v3 task contract), so
the cache captures a declaration instead of guessing from a directory snapshot.
Every declared output has exactly one owning task:

| Task | Owns | Root |
|------|------|------|
| `build-generate` | `.gen/`, except the ceded subpaths listed below | project dir |
| `build-describe` | `.gen/schema/`, `.gen/clientgen/`, `.gen/design/`, `.gen/migrations.json`, `.gen/migration-bundle/`, and the generated client directory (via the `clientOutputs` port, with `drift: "fail"`: the committed client is compared with the bytes present before this task wrote it, and a difference fails the task with `generated-output-drift` — commit the regenerated client) | project dir |
| `config-merge-exec` | `.gen/conf/.env.<APP_ENV or local>.yaml` (via the `mergedConfig` port) | project dir |
| `config-merge-test-exec` | `.gen/conf/.env.test.yaml` | project dir |
| `build-compile` | `bin/`, `VERSION` | per-command output dir (`build` only) |
| `build-cross-compile` | `bin/`, `VERSION` | per-command output dir (`package` only) |
| `test-exec` | `coverage.out`, `coverage.html` | per-command output dir |
| `package-go` | `go/` | per-command output dir |
| `package-archives` | `archives/` | per-command output dir |
| `package-docker` | `docker/` | per-command output dir |
| `config-extract-exec` | `schema/config.json`, `schema/config.jsonschema.json` | project dir |

Every output above except `.gen/` may legitimately be absent after a successful
run — coverage is not instrumented for a project with `coverage: false`, a project with no
client generator emits no client, a dry-run package writes no channel directory,
a project that contributes no migration operation has no bundle, a project with
nothing to merge writes no merged config file,
and a library's `bin/` is empty because its build evidence is a compile check.

`build-generate` does NOT own every subpath of `.gen`. It declares the subtree
with nine paths excluded. Seven of them exist because it runs FIRST and its
snapshot predates everything `build-describe` writes there — so a run serving
both tasks from cache used to restore a `.gen` missing all of it; five of those
`build-describe` declares, at the documented contract path rather than a
private staging copy. `.gen/deployment.json` is ceded for the same reason: the
`package` command's `deployment` step writes it after generate's snapshot and
declares it as its one required output (see
[Package](package.md#deployment-channel)). The ninth, `.gen/conf/`, is ceded for
the opposite ordering: `config-merge` runs BEFORE generate, so generate's snapshot adopted
whatever merged file was on disk at capture and its restore deleted the file
whenever the snapshot lacked it, while a `config-merge` cache hit reproduced
nothing because the task declared no output:

| Ceded subpath | Owner | The later reader that made it a defect |
|---|---|---|
| `.gen/schema/` | `build-describe` | `@putnami/clientgen` discovery and `go/framework/api` read `.gen/schema/openapi.json` in PREFERENCE to the committed sidecar; both clientgen tasks name it among their cache-key files |
| `.gen/clientgen/` | `build-describe` | the same discovery reads `.gen/clientgen/config.json` for generation; the `validate` guard reads committed inputs |
| `.gen/design/` | `build-describe` | `@putnami/sdd`'s feature projection and `putnami context` read `.gen/design/graph.json` |
| `.gen/migrations.json` | `build-describe` | none; ceded so a project that stops contributing migrations cannot have a stale dump resurrected |
| `.gen/migration-bundle/` | `build-describe` | release-set migration publication, deploy, `database.ApplyBundle`, the database test provider |
| `.gen/deployment.json` | `package-deployment` | the publication of the workload's release-set member of kind `deployment` |
| `.gen/conf/` | `config-merge-test-exec` (`.gen/conf/.env.test.yaml`) and `config-merge-exec` (`.gen/conf/.env.<APP_ENV or local>.yaml`, via the `mergedConfig` port) | `test-exec`'s database binding fallback reads `.gen/conf/.env.test.yaml`; every dependent's `config-merge` reads its dependencies' merged file. The `.manifest.json` sidecar beside them is claimed by nobody: it carries a `generatedAt` timestamp and no consumer reads it |
| `.gen/.describe.lock` | nobody | none: a run-scoped `lockedfile` mutex |
| `.gen/config-deps.json` | nobody | none: a fragment the same describe run folds into `schema/config.json` |

A ceded subpath no task claims is captured by nobody, and
[protocol ADR 0003](../../../protocols/extension/doc/adr/0003-a-declared-directory-output-may-cede-one-subpath.md)
says that is the declaration's statement rather than a defect. Both unclaimed
rows are there for determinism: their presence in generate's entry depended on
whether the tree happened to be clean, which alone makes a `cache.deterministic`
task disagree with itself between two equivalent runs.

The two `config-merge` claims name two different files inside one ceded
directory, never the directory itself. The test variant declares its file by
its literal path because it pins `APP_ENV=test`; the build variant reads
`APP_ENV` from the ambient environment, so its path is declarable only through
the `mergedConfig` port. Only one of them may use the port: the plan-time owner
check compares two port-backed outputs by port name under one project, and both
tasks are planned for every project under `build,test`. Both tasks also key on
the dependency closure's `conf/.env.yaml` and `conf/.env.*.yaml` (a `closure`
input), because the merged file is a function of the transitive closure's
source config — a key blind to a dependency's change would restore a stale
merged file on every later hit. See
[ADR 0005](adr/0005-describe-owns-the-migration-bundle-inside-gen.md).

`.gen/schema/` is the one ceded region with TWO producers, resolved as
**generate stages, describe converges**: `build-generate` writes its static
output both to `.gen/schema/` and to `.gen/generate-staging/schema/`, a mirror it
still owns, and `build-describe` rebuilds `.gen/schema/` from that mirror before
it snapshots and merges. That keeps describe's starting tree identical whether
generate executed in this invocation or was served from cache. The mirror is an
internal hand-off between two tasks of this extension: `.gen/schema/<rel>` stays
the only path a consumer ever resolves.

The rebuild REMOVES `.gen/schema/` first. Declared capture walks the declared
directory rather than the job's artifact list, so an artifact whose producer was
deleted from the app — a dropped `proto.New(...)`, an `openapi` plugin that no
longer configures — would otherwise be captured under this run's key and restored
by every later hit on it. A describer that still runs drops its own stale output;
the rebuild covers the one that does not run at all.

Because `.gen/schema/` is owned by a task that runs only for applications, a
project with **no describe phase may not set `options.generate.schema=false`**.
Nothing would restore its `.gen/schema/`, so the suppressed sidecar would leave
the contract with no durable home; `build-generate` refuses the combination and
names the option, the project, the artifact and the reason. The refusal is raised
only when a contract is actually about to be written: the option is inherited, so
a workspace-level default must not fail every library and CLI that generates
nothing. See
[ADR 0005](adr/0005-describe-owns-the-migration-bundle-inside-gen.md).

`build-compile` and `build-cross-compile` declare the same two paths. That is
legal because they never appear in one command, so the per-command output
directory each resolves to is a different directory.

Three footprints are deliberately **not** declared:

- **Everything else written under `<project>/.gen`.** `build-generate` is its
  single producer and owns the subtree whole apart from the ceded subpaths
  above; `config-extract`'s `.gen/config-schema.json` fallback and
  `build-infra`'s `.gen/requirements.json` write into generate-owned territory
  rather than claiming a slice of it. Two shared regions stay generate's
  deliberately: `.gen/infra/`, whose fragments
  `build-generate` deletes at the start of every run — so its entry is a pure
  function of its own producers — and whose durable form is the committed
  `infra/requirements.json`, and `.gen/generate-result.json`, whose only reader is
  `build-describe`, before describe rewrites it. What that costs is worth naming:
  a write inside `.gen` survives a cache hit only because generate's own snapshot
  already contained it, so anything a later task produces there and a later task
  reads has to be ceded and claimed.
- **The committed schema and infra sidecars** — `schema/openapi.json`,
  `schema/capabilities.json`, `infra/requirements.json`. These are tracked
  source files whose writer is chosen at runtime by the describe-sole-committer
  rule: `build-describe` commits them for app projects,
  `build-generate` for library and CLI projects. No static declaration can say
  "whichever of these two runs", and naming either would be a false exclusivity
  claim. The converged build-time copy of each lives under `.gen/schema/`, which
  `build-describe` owns. `schema/config.json` is the one exception: it has a
  dedicated producer task (`config-extract-exec`) whose cache hit has to
  reproduce it, so that task is its declared owner.

One known gap is recorded in the manifest rather than silently declared:
`build-tidy` rewrites `go.mod`/`go.sum` but does not declare `mutatesSources`
(that requires the project-scoped `sources` write resource, which would add plan
serialization edges). `serve-run` declares no outputs at all — it is
build-and-exec and a served process is not an artifact — and it declares the
`process` effect with `cache: false`, because a cache hit cannot reproduce a live
process or a bound port.

## Remote build cache

Go's build cache normally lives in a directory on the machine. Since Go 1.24 the
`GOCACHEPROG` variable can name a **helper program** the `go` command starts as a
child and delegates that cache to. This extension ships one, `putnami-go
gocacheprog`, so a machine that starts cold can be served compiled packages
another machine already built.

What it does, per lookup:

1. answers from the local cache under the Go cache root;
2. on a miss, asks the run's cache provider for the object, verifies the bytes
   against the digest it was announced under, and installs them locally. The
   lookups a parallel build issues at the same time travel as one request;
3. after a compile, writes the object locally, answers the compiler immediately,
   and offers the object to the provider in the background.

Every exchange with the provider has a deadline (5 s to connect, 15 s per
lookup, 60 s per upload). A provider that is absent, that hangs up, or that
stops answering is abandoned at the first failure and the helper finishes the
`go` command from the local directory alone; a build never waits on it.

### When it is active

Both of these must hold:

- the run has a **cache provider** that serves an object cache. The orchestrator
  hands every job the provider's socket; there is no socket under `--no-cache`,
  under `--cache-trust none`, or when no provider serves the run, and the helper
  is then not used at all;
- `--remote-build-cache` is on (the default). Pass
  `--remote-build-cache=false` to use the plain local Go cache for a run.

`GOCACHE` and `GOMODCACHE` keep the values they always had. The module cache is
untouched — the helper caches compiled output, not downloads — and `GOCACHE`
is where the helper keeps the compiled objects.

### Trust

Objects carry the provider's trust channel, exactly like task cache entries. A
run under `--cache-trust ci` reads only objects the provider stamped `trusted`;
everything else is a miss and the action is recompiled. A run that asserts no
policy gets the same strict filter: the helper opens up only for the explicit
`--cache-trust any`.

### What is shared, and what is not

A Go action id hashes the toolchain, the build flags, the target platform and
the source files — but also, without `-trimpath`, the **absolute directory** of a
package that is not in `GOROOT`. So the standard library is shared between any
two machines, while a workspace's own packages and its module dependencies are
shared only between checkouts whose paths match. Nothing is ever served for a
different input: a path that differs is a miss, not a wrong hit.

### Where the objects live, and who collects them

The local half spans two directories under the root described in
[getting started](./getting-started.md#toolchain-management):

- compiled objects live in `<cache root>/build`, the `GOCACHE` directory, in the
  `go` command's own format. A `go` command that runs without the helper (a run
  with no provider, a `--no-cache` run) finds them there, so each object is
  stored once on a machine, whichever kind of run compiled it first;
- the helper's lookup records live in `<cache root>/prog`. They are small, and
  they stay apart because the `go` command keeps its own records in `GOCACHE`
  under the same names but in a different format.

Both directories are swept by the same collector: `putnami cache clean` empties
them for a cold rebuild and `putnami cache gc` counts them against the
machine-wide budget.

### Diagnosing it

Export `PUTNAMI_DEBUG=1` in the environment of the `putnami` command (the CLI's
`--debug` flag does not reach the `go` processes a job spawns). Each `go`
invocation then reports what it traded, and a socket that could not be reached
or answer in time is reported once:

```
putnami-go gocacheprog: object cache served 182 objects, offered 3
```

## API Reference

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--skip-deps` | `false` | Skip `go mod tidy` and dependency sync |
| `--readonly` | `false` | Use `--mod readonly` (same as `--mod readonly`) |
| `--mod <mode>` | — | Module download mode: `readonly`, `vendor`, or `mod` |
| `--target` / `-t` | — | Cross-compile for one platform instead of the host: `os/arch` (e.g. `linux/amd64`). Wins over `platforms` |
| `--platforms` | host | Cross-compile for an explicit set instead of the host: `host`, `all`, or a comma-separated list. Also settable per project as `options["@putnami/go"].platforms` |
| `--output_path` / `-o` | — | Output binary path (absolute, or relative to job output dir) |
| `--entrypoint` | — | Package path to build (e.g. `./cmd/myapp`). Default: auto-detect |
| `--describe-entrypoint` | — | Package path to run for build-time describe/OpenAPI generation |
| `--install` | — | Copy compiled binary to this path (relative to workspace root) |
| `--ldflags` | — | Linker flags (e.g. `-s -w -X main.version=1.0`) |
| `--gcflags` | — | Go compiler flags (e.g. `-N -l` for debugging) |
| `--asmflags` | — | Go assembler flags |
| `--tags` | — | Build tags, comma-separated |
| `--race` | `false` | Enable race detector |
| `--trimpath` | `false` | Remove local file system paths from binary |
| `--cgo` | auto | Enable CGO (disabled automatically when cross-compiling) |
| `--buildvcs` | auto | Stamp version control information (revision, commit time, modified state) into the binary. Absent keeps Go's default, which stamps it inside a repository. `false` builds the same bytes at every commit whose compiled sources are unchanged; together with no `version-var`, the binary does not change between builds that differ only in their version |
| `--buildmode` | `default` | Build mode: `default`, `archive`, `c-archive`, `c-shared`, `shared`, `exe`, `pie` |
| `--installsuffix` | — | Install suffix for build cache isolation |
| `--p <n>` | GOMAXPROCS | Number of parallel compilations |
| `--remote-build-cache` | `true` | Back the Go build cache with the run's shared object cache when a cache provider offers one. `--remote-build-cache=false` keeps the local Go cache alone. See [Remote build cache](#remote-build-cache) |

### Emitted metrics

| Metric | Unit | Description |
|--------|------|-------------|
| `binary-size` | bytes | Size of the compiled binary |

## Task deadline

`build-cross-compile` (the `package` pipeline's compile step) compiles every
entrypoint once per archive platform, sequentially, which makes it this
extension's longest single-tool task. Its scheduler deadline is
**900000 ms (15 min)**, raised from 600000 ms.

`build-compile` (the `build` pipeline's compile step) holds the **same
900000 ms** ceiling, raised from 300000 ms. Its ordinary work shrank — the host
platform alone — but an explicit `platforms` request can still ask it for the
full matrix, and a ceiling sized for the default would terminate exactly the
invocation that asked for the most work. A deadline is sized by what a task can
be asked to do, not by its median.

`build-describe` holds the **same 900000 ms** ceiling, raised from 300000 ms.
Not from a measurement: describe runs the same `go build` of the same entrypoint
as `build-compile`, with the same configuration, and then runs the
binary it produced, under its own 60 s cap. Its work contains compile's, so a
lower ceiling would terminate describe on a tree where compile is allowed to
finish. On Linux the direction matters: describe now builds the cgo variant that
compile used to build a second time, so the pair costs less overall and
describe's own share is the larger of the two.

`go build` exposes no timeout of its own, so the scheduler deadline is the only
one — a run that exceeds it is terminated by the orchestrator, not by the
toolchain.

### Why 900000 ms

The previous 600000 ms ceiling was reached *exactly* in production, and the same
commit then passed on retry in 213 s. Ending at the ceiling and passing on retry
is the signature of an undersized deadline rather than a stable repository
verdict.

Measurement set (`n` is too small to call a p95, so these are observed
**maxima**; measured 2026-08-03 on a 10-core Apple Silicon laptop against
`tooling/cli`, 167,897 Go LOC, four archive platforms):

| Run | Cache state | Duration |
|-----|-------------|----------|
| Four-platform `go build`, 10 cores | scratch `GOCACHE` (cold) | 75.3 s |
| Four-platform `go build`, `GOMAXPROCS=2`, `-p 2` | scratch `GOCACHE` (cold) | 54.7 s |
| `putnami build --no-cache` × 2 (`@putnami/cli`) † | warm toolchain cache | 13.6 s, 4.6 s |
| `putnami build --no-cache` (`@putnami/go`) † | warm toolchain cache | 14.4 s |
| Production `build-cross-compile`, passing retry | cloud runner | 213 s |

† whole-command wall time, an upper bound on the cross-compile step alone (the
step itself reported 2.5–2.6 s in those runs).

`n = 5` local runs plus the one production pass. 900000 ms is ~4.2× the 213 s
maximum and ~12× the local cold-cache maximum.

> **Cloud activation.** Raising the deadline in this extension does not change
> anything for a Cloud consumer until that consumer upgrades its pinned
> `@putnami/go` version in its lock; the runner reads the deadline from the
> locked extension manifest. Publication and the Cloud lock/runner rollout are
> tracked in the downstream qualification issue, not here. The rollback path is
> the same lever in reverse: pin the previous extension version.

## Boundaries

- **Scope**: project-only — workspace-level runs are not supported
- **Platforms**: the host alone unless `--target` or `platforms` says otherwise; the release matrix is [`package`](./package.md)'s, not `build`'s
- **No auto-widening**: build tags, CGO, assembly, and platform-specific sources never add platforms implicitly — declare them
- **Libraries**: emit no binary; standalone/explicit builds compile-check, while
  the ordinary compile in a combined `lint,test,build` invocation reuses the
  same-invocation test+lint evidence only when compile-affecting race settings
  match
- **CGO**: disabled by default for cross-compilation; CGO cross-compilation requires a matching C cross-compiler in PATH
- **Multiple binaries**: when `cmd/*/main.go` are detected, each subdirectory produces a separate binary
- **Dependencies**: uses the workspace's managed Go toolchain; see [Getting Started](./getting-started.md) for toolchain details
