# Build

The build command compiles TypeScript projects through a 5-phase pipeline: **generate**, **transpile**, **types**, **compile**, and **infra**. The first four are independently cacheable and can be run selectively; **infra** aggregates a workload's deployability requirements at the end of the build and is cached on the files it reads.

## Overview

- Generates code artifacts and version metadata before compilation
- Transpiles TypeScript to JavaScript using Bun's bundler
- Produces TypeScript declaration files (`.d.ts`) using `tsc`
- Optionally compiles to standalone executables for cross-platform deployment
- Caches each phase independently with content-hash-based cache keys
- Batches each producer phase across projects that are ready at the same time, so one `putnami-ts` process handles several projects and amortizes extension startup; every project keeps its own cache entry, outputs, and terminal row

## Usage

### Basic Usage

```bash
# Default build: generate + transpile + types
putnami build .
```

This runs the three default phases and writes output to `.putnami/projects/<project>/@putnami-typescript/build/latest/output/`.

### Common Patterns

```bash
# Build with compiled binary (all 4 phases)
putnami build . --compile

# Fast local build (skip type declarations)
putnami build . --fast

# Bundle everything into the output (no externals)
putnami build . --bundle bundled

# Release build (strips pre-release version suffix)
putnami build . --release

# Build for Node.js target instead of Bun
putnami build . --target node
```

### Advanced Usage

```bash
# Run only specific phases
putnami build . --transpile              # Transpile only
putnami build . --types                  # Types only
putnami build . --compile                # Compile only
putnami build . --transpile --types      # Transpile + types (no generate)

# Cross-compile for a single platform
putnami build . --compile --compile-target bun-linux-x64

# Clear generated files before building
putnami build . --clear

# Skip output package.json and artifact copying
putnami build . --skip-package-json --skip-artifacts
```

## Pipeline

```text
generate ──→ transpile ──→ types ──→ infra
         └──→ compile  (parallel with transpile/types)
```

### Phase Selection Logic

- **No phase flags set**: generate + transpile + types run (compile is opt-in)
- **Any phase flag set** (`--transpile`, `--types`, `--compile`): only the selected phases run
- Generate always runs unless only downstream phases are selected

### Phase 1: Generate

Prepares the `.gen/` directory with generated code, version metadata, and pre-build hook outputs.

Steps:

1. **Content hash** — computes SHA256 of all `src/` TypeScript files (first 12 chars)
2. **Prepare `.gen/`** — creates or clears the `.gen/` directory
3. **Copy assets** — copies files declared in `putnami.json` `options.generate.assets` (or legacy `.putnamirc.json`). A missing source is skipped with a warning; a copy that fails, such as a symlink to a directory inside a directory asset, fails the generate
4. **Pre-build hooks** — discovers and runs hooks from dependencies that declare `preBuild` in their `putnami.extension.json`. Hooks can register exports and assets. They run in ascending `hooks.preBuild.order`, ties broken by extension name — see [hook order](#pre-build-hook-order).
5. **HTTP route inventory** — merges application and web framework facts into `.gen/schema/http-routes.json`
6. **Infra requirements sync** — reconciles the committed `<project>/infra/requirements.json` from the `.gen/infra/*.json` scratch fragments the hooks just wrote — see [infra requirements ordering](#infra-requirements-ordering)
7. **Version info** — writes/updates `.gen/version.json` with the `contentHash` field
8. **Bundled serve** — if any hook registered `*-loader` exports, generates a bundled serve entrypoint that registers server-side loaders and starts the app

#### Infra requirements ordering

`build-generate` is the **only** task that writes the committed
`<project>/infra/requirements.json`. It brackets the pre-build hooks:

1. Before the hooks run, it deletes every `<project>/.gen/infra/*.json` scratch
   fragment, so a producer removed from the source cannot leave a requirement
   behind.
2. Each hook writes its own fragment slug — `@putnami/application` writes
   `secrets.json`, the database, migration, storage and event producers write
   theirs.
3. After the **last** hook returns, one sync merges every fragment into the
   committed manifest, deterministically and sorted. When no fragment declares a
   resource, a stale committed manifest is deleted.

The converged producer set exists only at step 3, and it exists there because
`@putnami/application`'s hook activates the workload's **complete** config
registry before it emits `secrets.json`: it imports the generated `*-loader`
modules (which is what runs a route handler's `configToken()` calls) and walks
the plugin tree for `ConfigContributor` blocks. The walk used to be missing, so
the committed manifest dropped every secret a **dependency** declared while
`schema/config.json` listed it — see
`typescript/framework/application/bin/_activate-config.ts`.

The walk is skipped, with a warning naming the cause and the remedy, when the
discovered app predates it. A workload that bundles its own copy of
`@putnami/application` hands the hook an app from a sibling module realm, and
that shape is supported by design — so this step degrades rather than failing a
build that has no way to satisfy it.

`config-extract` never reconciles the committed manifest. It rewrites
`.gen/infra/secrets.json` from the same activated registry, so it can only
reproduce the bytes `build-generate` already committed; `putnami build` does not
schedule it at all.

This is where TypeScript diverges from Go. The Go extension defers this sync to
its `describe` phase, because its `build-generate` sees only the statically
derived fragments while the runtime-derived ones land later. TypeScript has no
such split: the pre-build hook runs `Application.build()` on the same plugin tree
the runtime composes, so what the hooks emit **is** the converged set.

Manifests carry canonical secret **names** only (`integrations.stripe.api_key`),
never values. The name grammar is lowercase snake_case, so a camelCase config
field is canonicalized on the way out.

The scheduler caches the generated `.gen/` tree as the generate task output and restores it in place on cache hits.

#### Pre-build hook order

Every `preBuild` hook for a project writes into the same `.gen/` tree, so a hook
only sees what the hooks before it produced. The sequence is therefore fixed by
the manifests, never by discovery order: hooks run in ascending
`hooks.preBuild.order` (default `0`), ties broken by extension name in byte
order.

| Extension | `order` | Why |
|-----------|---------|-----|
| `@putnami/web` (and other generators) | `0` | Producer: writes generated sources such as the React server loaders. |
| `@putnami/application` | `100` | Finalizer: imports the workload entry point — which may import a generated module — and emits `schema/capabilities.json` describing the finished tree. |

Inverting this pair breaks the build: on a fresh tree
`@putnami/application`'s entry-point import fails against loaders that do not
exist yet, the hook reports no capability manifest, and step 8 fails with
`generated server loaders require schema/capabilities.json`. A warm `.gen/`
hides it, which is why it only ever reproduced on cold CI checkouts.

#### Pre-build hook environment

The extension runs each hook in the project directory with these variables set:

| Variable | Value |
|----------|-------|
| `PUTNAMI_PROJECT_ROOT` | Absolute path of the project being built. |
| `PUTNAMI_WORKSPACE_ROOT` | Absolute path of the workspace root. |
| `PUTNAMI_PREBUILD_CONTEXT` | `true`: the process runs as a pre-build hook, not as the workload. |

`runHookCommand` from `@putnami/utils` sets the same three variables from the
hook context it reads, so a hook sees them whichever way it was launched.

When the checked-out framework is newer than the running scheduler, a complete
pre-source-binding scheduler stamp produces a protocol-v1 activation manifest
without feature evidence. The extension reads that manifest through the same
v1/v2 activation projection, keeping this ordering and loader set unchanged
until the scheduler can stamp v2 source provenance.

The route inventory uses `putnami.http-routes.v1`. It includes typed API
endpoints, file-system pages and actions, bounded hashed-asset prefixes, and
exact root public files. Build hooks contribute fragments independently; the
extension canonicalizes them after every hook, so hook order cannot change the
artifact or its digest. Unsupported catch-alls and ambiguous static behavior
fail with an `http_routes.*` diagnostic rather than producing a broad `/*`
route.

### Phase 2: Transpile

Invokes `bun build` to transpile TypeScript to JavaScript.

**Entrypoint resolution** (in order):

1. All paths from `package.json` `exports` field (all conditions)
2. `src/main.ts`
3. `src/index.ts`
4. `src/lib.ts`

Plus every existing `bin` target, so published executables ship as `.js`.
Entrypoints are ordered deterministically (export key, then condition, in byte
order) because that ordering drives bun's chunk layout.

#### Build graph partition

Resolved entrypoints are partitioned by export condition and each partition is
a **separate `bun build` invocation**:

| Graph | Entrypoints | Target |
|-------|-------------|--------|
| `server` | Everything reached through a non-browser condition (`default`, `node`, `bun`, ...), the `src/main.ts`/`src/index.ts`/`src/lib.ts` fallbacks, and every `bin` target | `--target` (default `bun`) |
| `browser` | Entrypoints reached **only** through a browser-family condition (`browser`, `react-native`) | always `browser` |

This is the publication boundary between browser and server code. `--splitting`
emits shared chunks across the entrypoints of *one* invocation, so a package
whose server entry and browser entry both import the same client module would
otherwise get a shared chunk that the browser entry imports and that also
carries server-only code — the failure mode a shared browser/server graph produces.
Splitting the invocations removes that channel structurally; per-module
`browser` mappings and hand-written browser stubs cannot.

Rules:

- A package with no browser-family condition produces exactly **one**
  invocation, identical to the single-graph behaviour — no extra process for
  the common case.
- An entrypoint reachable from *both* a browser and a non-browser condition
  stays on the server graph. It is never silently relocated and never built
  twice.
- `react-native` entries join the browser graph: bun has no react-native
  target, and it is the only isolated non-server graph the bundler offers.
- The browser graph is built with `--target browser`, so bun applies browser
  resolution: the `browser` export condition of bundled dependencies **and**
  the package's own top-level `browser` field mapping.
- Both invocations share `--root <project>` and `--outdir`, so emitted entry
  paths land exactly where the published `exports` expect them
  (`src/index.js`, `src/index.browser.js`). Chunk filenames are content-hashed,
  so the two graphs can only agree on a name when the bytes are identical.
- A failure names the graph it came from
  (`Transpile failed [browser graph]: ...`); a server-graph failure short-circuits
  before the browser graph runs.

**Builder environment**: every invocation passes `--env=disable`, so the
output does not depend on the machine that built it:

- JSX compiles to the production automatic runtime (`jsx`/`jsxs` from
  `react/jsx-runtime`). Without the flag, Bun emits `jsxDEV` from
  `react/jsx-dev-runtime` when `NODE_ENV` is not `production` or the project
  has a `bunfig.toml`, and a production consumer bundle resolves `jsxDEV` to
  `undefined`.
- `process.env` reads, `NODE_ENV` included, stay runtime reads. The consumer's
  environment, or its bundler's define, decides their value.

**External resolution**: dependencies and peerDependencies are marked as external unless `--bundle bundled` is set. Both graphs use the same external set, so consumers still resolve dependency conditions themselves.

**TSConfig resolution** (in order):

1. `tsconfig.app.json` in project
2. `tsconfig.lib.json` in project
3. `tsconfig.json` in project
4. `tsconfig.json` in workspace root

Output is written to `output/lib/`.

### Phase 3: Types

Invokes `tsc` to generate TypeScript declaration files (`.d.ts` + `.d.ts.map`).

`tsc` starts through `bun x tsc`. On a host with a `node` on `PATH`, Bun runs `tsc` with that `node`, which type-checks faster and with less memory than Bun. On a host without one, Bun runs `tsc` itself. The build never needs Node.js, and Putnami never installs it.

TSC is called with:
- `--declaration --emitDeclarationOnly --declarationMap`
- `--strict true --target esnext --module esnext`
- `--moduleResolution bundler --jsx react-jsx`
- `--types bun --lib ESNext,DOM`

Source files are discovered from `src/` and `bin/` directories (excluding `.d.ts` and test files).

When the project has a tsconfig, tsc runs with a temporary config that extends it and includes only `src/`, `bin/` and `.gen/`, without test files. That config lives in the task's cache directory, never in the project, because a project directory can lie inside another task's declared output, such as a generated client inside its provider's `clientgen` output. It resolves as it would from the project directory: its paths are absolute, `${configDir}` in the extends chain means the project directory, and type roots are the chain's own or the `node_modules/@types` directories of the project and its ancestors. A `tsconfig.types.*.json` an earlier release left in the project is removed.

Output is written to `output/types/`.

### Phase 4: Compile

Invokes `bun build --compile` to produce standalone executables.

**Entrypoint resolution** (in order):

1. `bundled-serve` from generate result (if loaders were registered)
2. `./serve` export from `package.json`
3. `bin` field from `package.json`
4. `main` field from `package.json`

**Default targets** — the distribution matrix an unbound compile produces:

| Target | Output |
|--------|--------|
| `bun-linux-x64` | `<name>-linux-x64` |
| `bun-linux-arm64` | `<name>-linux-arm64` |
| `bun-darwin-x64` | `<name>-darwin-x64` |
| `bun-darwin-arm64` | `<name>-darwin-arm64` |
| `bun-windows-x64` | `<name>-windows-x64.exe` |

Use `--compile-target` to build for a single platform.

The matrix is **not** what a channel-bound compile produces: under
`package --docker` this phase compiles exactly the image's target (see
[Package](./07-package.md#platform-mapping)), because an image carries one
executable. Both deciding inputs are plan-time parameters that participate in
the phase's cache key.

Output is written to `output/compile/`.

### Phase 5: Infra (workloads only)

Emits the workload's deployability manifest at `<project>/.gen/requirements.json`:

1. Merges the committed `infra/requirements.json` of every project in the
   workload's dependency closure (the closure arrives on the job context).
2. Applies `<workload>/infra/overrides.json`.
3. Resolves the runtime block — an authored `<workload>/infra/runtime.json` wins
   and its stale defaults sidecar is removed; otherwise framework defaults are
   synthesized into `<workload>/.gen/infra/runtime.json`.
4. Turns HTTP/2 **off** in that block, whatever its source: Bun does not serve
   h2c, and the deploy target may default a service to HTTP/2, so a TypeScript
   workload has to opt out explicitly. This is the TypeScript extension's own
   statement since an earlier migration; the CLI used to infer it from a
   project's tags.
5. Writes the manifest atomically.

Findings (a malformed contribution, a merge conflict, an unused override) are
reported as task warnings and never fail the build. The phase is skipped for
libraries.

The phase is cached. Its key holds every file it reads: the committed
`infra/requirements.json` of every project in the closure, with that project's
path, and the workload's `infra/runtime.json` and `infra/overrides.json`. A
change to a dependency's requirements, or a project that joins or leaves the
closure, moves the key. The key also moves with the project type and with the
extension's code, which holds the runtime defaults and the HTTP/2 rule. No
commit, ref or checkout path reaches the key. A cache hit restores
`.gen/requirements.json` and, without an authored runtime,
`.gen/infra/runtime.json`. Only a run that writes the manifest is stored: a
library, a workload that declares nothing and a run whose write failed report a
skip and are not cached. A cache hit does not repeat the findings.

The `package` command's `deployment` step writes the same aggregate, under the
same HTTP/2 hook, as the workload's deployment declaration (see
[Package → Deployment Channel](./07-package.md#deployment-channel)).

## Output Layout

Every step of one command shares a single output directory,
`.putnami/out/<project>/<command>/`, and each step owns an exact subpath of it:

```text
.putnami/out/<project>/build/
├── lib/                    # Transpiled JavaScript — owned by the transpile step
│   ├── *.js
│   ├── *.js.map
│   └── ...
├── types/                  # TypeScript declarations — owned by the types step
│   ├── *.d.ts
│   ├── *.d.ts.map
│   └── ...
└── compile/                # Standalone executables — owned by the compile step
    ├── serve-linux-x64
    ├── serve-linux-arm64
    ├── serve-darwin-x64
    └── serve-darwin-arm64
```

## Declared Outputs

The manifest states each task's filesystem footprint (the v3 task contract), so
the cache captures a declaration instead of guessing from a directory snapshot.
Every output has exactly one owning task:

| Task | Owns | Root |
|------|------|------|
| `build-generate` | `.gen/` minus `.gen/deployment.json`, `.gen/requirements.json` and `.gen/infra/runtime.json`, `schema/capabilities.json`, `schema/openapi.json` (with `drift: "fail"`: a committed spec that differs from the one `openapi()` regenerates fails the task with `generated-output-drift` — commit the regenerated spec), and the generated client directory (via the `clientOutput` port, with `drift: "fail"`: the committed client is compared with the bytes present before this task wrote it, and a difference fails the task with `generated-output-drift` — commit the regenerated client) | project dir |
| `build-transpile` | `lib/` | per-command output dir |
| `build-types` | `types/` | per-command output dir |
| `build-compile` | `compile/` | per-command output dir |
| `test-run` | `lcov.info`, `results.junit.xml` | per-command output dir |
| `package-npm` | `npm/` | per-command output dir |
| `package-docker` | `docker/` | per-command output dir |
| `build-infra` | `.gen/requirements.json` (required: a run that writes none caches nothing), `.gen/infra/runtime.json` | project dir |
| `package-deployment` | `.gen/deployment.json` (required: a run that writes none caches nothing) | project dir |
| `config-extract-exec` | `schema/config.json`, `schema/config.jsonschema.json` | project dir |

Except the required `.gen/requirements.json` of `build-infra` and
`.gen/deployment.json` of `package-deployment`, each declared output above may
legitimately be absent after a successful run — a project with no entrypoint
transpiles to nothing, coverage is only instrumented under `--enforce-coverage`,
a project with no config blocks emits no schema, and a workload that authors
`infra/runtime.json` gets no runtime defaults file.

`build-generate` is the single producer of `<project>/.gen` — anything else
that writes inside `.gen` (such as the `config-extract` fallback location)
writes into generate-owned territory rather than claiming a slice of it. The
exceptions are the files later steps write after generate's snapshot:
`build-generate` cedes `.gen/deployment.json` to `package-deployment` (see
[Package → Deployment Channel](./07-package.md#deployment-channel)), and
`.gen/requirements.json` and `.gen/infra/runtime.json` to `build-infra`.
`build-generate` still deletes `.gen/infra/` fragments at the start of every
run, the runtime defaults file with them, and `build-infra` writes it again
after generate.

Every packaging task declares everything it writes, including the channel record
it leaves inside its own output directory, which is what keeps `package` under
ordinary declared capture
([ADR 0038](../../../tooling/cli/doc/adr/0038-package-tasks-use-ordinary-declared-capture.md)).

## API Reference

### Flags

#### Phase Control

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--transpile` | `boolean` | `false` | Run only the transpile phase |
| `--types` | `boolean` | `false` | Run only the types phase |
| `--compile` | `boolean` | `false` | Run only the compile phase |

When no phase flag is set, generate + transpile + types run by default. Setting any phase flag switches to selective mode.

#### Transpile Options

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--target` | `string` | `bun` | Build target platform: `bun`, `node`, or `browser` |
| `--bundle` | `string` | `none` | Bundle mode. `none`: externalize deps. `bundled`: inline everything. `local`: externalize only non-workspace deps |
| `--sourcemap` | `string` | `external` | Sourcemap generation: `external` (separate `.map` files), `inline` (embedded), or `none` |
| `--minify` | `boolean` | `true` | Minify output JavaScript |
| `--splitting` | `boolean` | `true` | Enable code splitting (produces ESM with shared chunks) |

#### Compile Options

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--compile-target` | `string` | — | Build for a single platform (e.g., `bun-linux-x64`) instead of all 4 defaults |

#### Output Controls

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--fast` | `boolean` | `false` | Skip TypeScript declaration generation (alias for running without types) |
| `--clear` | `boolean` | `false` | Clear `.gen/` directory before generating |
| `--skip-package-json` | `boolean` | `false` | Skip generating output `package.json` |
| `--skip-artifacts` | `boolean` | `false` | Skip copying build artifacts to output |
| `--release` | `boolean` | `false` | Build for stable release (strips pre-release version suffix) |

#### Version Controls

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--version-suffix` | `string` | — | Override the auto-generated version suffix |
| `--publish-config-access` | `string` | `public` | Package access level for npm (`public` or `restricted`) |

## Boundaries

- **Scope**: Compiling TypeScript projects into distributable JavaScript, type declarations, and standalone executables
- **Out of scope**: Publishing (see [Package](./07-package.md)), running tests (see [Test](./03-test.md)), code quality (see [Lint](./04-lint.md))
- **Dependencies**: Requires Bun runtime. Uses `tsc` from `node_modules` for type generation.
- **Extension points**: Pre-build hooks allow dependencies to inject generated code and register exports. Configure via `putnami.extension.json` `hooks.preBuild`.
