# @putnami/typescript

TypeScript extension for the Putnami workspace. Provides build, test, lint,
serve, run, package, workspace-fetch and workspace-install jobs for TypeScript projects, using
Bun as the runtime and bundler and Biome as the formatter and linter. It is
implemented as a Go binary (`putnami-ts`) that the v3 extension manifest
declares as `{extensionRuntime}`.

## Contract before code

`@putnami/typescript` is classified `stable` in the workspace
`putnami.support.json`. Before changing behaviour here, read what the change is
allowed to move:

| Artifact | Holds |
| -------- | ----- |
| [`specs/typescript-project-toolchain.json`](specs/typescript-project-toolchain.json) | The observable requirements, each protected by named tests |
| [`doc/adr/0001-…`](doc/adr/0001-batched-phases-must-equal-solo-runs.md) | Why a batched phase must produce exactly what the solo runs produced |
| [`doc/adr/0002-…`](doc/adr/0002-one-workspace-catalog-owns-shared-versions.md) | Why one workspace catalog owns shared versions, and why install seeds it |
| [`doc/adr/0003-…`](doc/adr/0003-a-package-must-load-before-it-ships.md) | Why packaging loads the staged package instead of trusting the build |

## Build pipeline

```text
generate ──→ transpile ──→ types
         └──→ compile
```

Each phase caches independently and can be batched across projects. Batching is
an execution strategy only: every batched phase writes each project's artifacts
into that project's own output directory — including the type-checker's
incremental build-info state — and attributes each failure to the project that
produced it. A `*Batch_EqualsSolo` test is the guard; a new phase is not
batchable until it has one.

## Invariants that are easy to break

- **Per-project output isolation.** Two projects sharing an output or
  build-info directory make declarations depend on execution order.
- **Failure isolation.** One project's compile error must not suppress a
  sibling's output, and every failure path must emit a diagnostic.
- **Catalog seeding is additive.** Never overwrite an existing catalog entry,
  never seed a non-`@putnami/*` package, and always preserve the root
  manifest's top-level key order when rewriting it — the order is recovered
  from the original bytes and threaded through the write.
- **Prove loadability.** A staged package that does not load under its claimed
  CLI contract is not shipped, a claim newer than the packager's is rejected,
  and a stale lower claim is ratcheted up.
- **`catalog:` has one reader.** `internal/catalog` is it. Install, upgrade and
  the npm publisher all resolve specifiers through that package.

## Skip guard

Lint refuses a focused test (`.only(`, `['only'](`, `fit(`), a `.skip(` or
`xit(` outside a guard, and a guard (`if`, `case`, `&&`, a ternary, `skipIf`)
whose condition names CI or flakiness. An uncalled `test.skip` that is
assigned, returned or chosen by a condition counts as a skip. Fix the test, or
guard the skip with `skipIf(<platform or dependency check>)`. A skip that
stays on purpose carries `// putnami:allow-skip <reason>` on its line or just
above; lint reports it as a warning. See
[doc/04-lint.md](doc/04-lint.md#skip-guard).

Lint also fails on a relative link or an anchor in the project's `README.md`
files and `doc/` trees that does not resolve, including a path whose case
differs from the file on disk. Fix the link; do not turn the check off. See
[doc/04-lint.md](doc/04-lint.md#documentation-links).

## Serve and run

Serve resolves its entrypoint from the project's `"./serve"` export unless one
is passed explicitly, and reports a user-facing error when there is no package
manifest or no such export — it never guesses a file. A served or run child
forwards its own exit code; an interrupt terminates the whole child process
group and reports 130, escalating to SIGKILL after the grace period.

## Templates

| Template | Description |
| -------- | ----------- |
| `typescript-library` | Library with a public entry point and a test that imports through it |
| `typescript-server` | HTTP server with file-based API routing and declared query/body input |
| `typescript-web` | React SSR app: file-routed pages, layout, error and not-found boundaries, a loader/action pair, one island |

They live in `typescript/templates/` and are packaged by `@putnami/scaffold`,
not by this extension. Each declares its own feature and spec; the contract they
are held to is in
`tooling/scaffold/internal/packaging/committed_templates_test.go`.

## Reference sample

`typescript/samples/03-web/` is the fullest example of the web surface;
`typescript/samples/02-rest-api/` of the API surface.
