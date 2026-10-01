# ADR 0006 — Each template is rendered and run against the workspace framework

- **Status**: accepted
- **Scope**: `@putnami/typescript-templates-proof` (`typescript/templates/proof`),
  `@putnami/go-templates-proof` (`go/templates/proof`)

## Context

[ADR 0002](0002-templates-are-checked-by-their-packager.md) checks the
committed templates statically. Some defects pass every static check and show
only when a rendered template compiles and runs against the framework: a
handler reading `ctx.query` as a value when it is a function, a test calling an
application method that no longer exists, a missing `@putnami/runtime`
dependency. [ADR 0003](0003-what-a-default-template-promises.md) promises a
working example and a test beside it; a static check cannot hold that promise.

## Decision

Each language that ships templates has one **template proof** beside them:
`typescript/templates/proof` and `go/templates/proof`, an ordinary test-only
project of its language. For every template in the same directory whose
`putnami.template.json` names the language's extension, its test:

1. renders the template with the variables `putnami dev template test` uses;
2. writes the result into a new temporary workspace wired to this repository's
   framework sources, not to a registry;
3. runs what `putnami lint`, `putnami build` and `putnami test` run on a freshly
   scaffolded project;
4. removes the temporary workspace.

| | TypeScript proof | Go proof |
|---|---|---|
| Wiring | `node_modules` links to the packages the proof installed: every `@putnami/*` package as `workspace:*` (framework source), every other package at the `bun.lock` version. The workspace `tsconfig.json` extends the extension's config. | A `go.work` listing every module of this repository's `go.work` plus the rendered module. |
| Steps | The preBuild hooks the rendered `package.json` activates; `biome check` with the extension's `config/biome.json` and the `.gitignore` `putnami init` writes, failing on a warning; the `build~types` type-check; `bun test`. | `go list -deps -test` to fill the module cache, then `go vet`, `go build`, `go test -count=1`, then golangci-lint with the extension's `config/.golangci.yml` and staticcheck, at the versions `go/extension/tools/versions.json` pins. |
| Offline | Reads installed packages only. | The module-cache step uses the proxy settings with `GOFLAGS=-mod=readonly`, so every download is checked against `go.sum`. Other steps run with `GOPROXY=off`, `GOENV=off`, `GOFLAGS=-mod=readonly`, `GOTOOLCHAIN=local`. No step inherits `GOCACHEPROG`, whose helper needs the `PUTNAMI_*` variables the proof drops. |

### The cache key covers the templates and the framework

The proof's test task is cached. Its key changes when a template or a framework
file the rendered project reads changes, and the same inputs select the proof
under `--impacted`.

- **TypeScript.** The proof's `package.json` declares every framework package a
  template uses as `workspace:*`, which puts each package's `generate` task
  upstream and covers its `src/**` and `package.json` transitively.
  `options.test.filePatterns` declares the rest: each template directory, the
  template manifests, the extension's `tsconfig.json` and `biome.json`, the CLI
  source holding the `.gitignore` `putnami init` writes, and the `bin/**` and
  `putnami.extension.json` of every package whose preBuild hook the proof runs.
- **Go.** A Go module cannot keep a `require` it does not import, so the proof
  declares no module edges. `options.test.filePatterns` declares each template
  directory, every framework and protocol module the rendered projects compile,
  the extension's `.golangci.yml` and `tools/versions.json`.

## Invariants

- **Discovery is by manifest.** A template added beside a proof is proven with
  no edit to the proof.
- **A test holds the key complete.** The TypeScript proof fails when a template
  declares a dependency the proof does not declare with the same specifier, or
  when a framework package's `src/**`, `package.json`, `bin/**` or
  `putnami.extension.json` is not a declared input. The Go proof fails when a
  module inside this repository that a rendered project compiles
  (`go list -deps -test`) is not a declared input. Both fail when a template
  directory is not a declared input.
- **Offline after one fetch.** The TypeScript proof reaches no registry. The Go
  proof fetches only the third-party modules the rendered project compiles, at
  the `go.sum` versions, because nothing orders it after a task that fills the
  module cache. No step needs credentials; every build step runs offline.
- **Nothing is left behind.** Every write lands under one temporary directory,
  removed after each template. The Go build and module caches and the linters'
  caches are the shared user caches.
- **A linter it cannot run fails.** The Go proof resolves each pinned linter as
  the lint job does (PATH, the machine tool home, the workspace's legacy tool
  directories) and fails, naming `putnami install`, when none holds the pinned
  version built with the local Go minor or newer. The TypeScript proof fails the
  same way when `@biomejs/biome` is not installed. Neither skips. On a machine
  missing a linter, the first `lint,test` run can fail the proof and the next
  passes, because the lint task installs it.
- **A lint that reads nothing fails.** Before the templates, the Go proof lints
  planted findings in packages named `build` and `bin`, in a module under
  directories named like each excluded path, from the module directory and
  through a symlink. It fails when golangci-lint misses one: exclusions matched
  against the directories above the project would pass every lint unread.
- **A leftover placeholder fails.** A `<%= name %>` surviving render fails.

## What the proofs do not prove

- **Rendering is replayed, not called.** The renderer is internal to
  `@putnami/cli`; each proof replays `protocols/template/doc/02-variables.md`.
  A change to those rules needs both replays updated.
- **Generated serve entries and packaging are not run.** The TypeScript proof
  does not generate `.gen/src/serve.bundled.ts`, which needs the `build~describe`
  capability manifest. The scaffold requirement
  `generated-imports-are-declared` holds the `@putnami/runtime` dependency
  statically.
- **Dependencies resolve to the workspace, not the registry.** The static
  `go.mod` check covers a Go template's require list; a TypeScript framework
  package resolves to its source, not its published build.
- **Only the linters are run**, not `putnami lint`'s skip guard or `lint-docs`.
- **Python templates are not proven.** A proof would add the Python toolchain
  to every gate that touches the framework.

## Rejected alternatives

- **Run `putnami dev template test` or a nested `putnami build`.** Both install
  extensions from the registry: a network, credentials and a nested CLI session.
- **Put the proof in `@putnami/scaffold`.** Its Go tests would need bun and tsc,
  and every framework edit would rerun the packaging tests.
- **Declare Go module edges.** `go mod tidy` removes them, and a blank import
  would be a fake dependency. The completeness check states the real closure.

## Consequences

- A change to a framework package a template uses reruns the proof: about 24 s
  cold for TypeScript and 46 s for Go on a loaded laptop, a replay otherwise.
- Adding a framework dependency to a template fails the proof until the proof
  declares it too.
- The render rules exist in three places: the CLI and the two proofs. The
  `.gitignore` `putnami init` writes exists in two, and a static test compares
  them.
