# ADR 0016 — A test run keys on the JavaScript its dependencies emit

- **Status**: accepted
- **Scope**: `@putnami/typescript` (`typescript/extension`): the `build-emit`
  task (`internal/emit`) and the dependencies of `test-run` in the `test`
  pipeline
- **Applies**: the artifact input of [CLI ADR 0043](../../../../tooling/cli/doc/adr/0043-a-cross-project-carrier-task-keys-only-on-what-it-reads.md),
  as amended for artifact inputs

## Context

A TypeScript package exports its `src/*.ts`, and Bun runs a dependent's tests
against those sources. Bun strips types and comments before it executes a
module. Yet `test-run` folded the keys of `^tsc`, `^tsc-auto` and its own
`generate`, and those keys read every file of each dependency. A type-only or
comment-only edit in a library therefore reran every dependent's tests.

Measured on this repository: after an edit in `@putnami/runtime`, which has 28
direct TypeScript dependents, `putnami test --tag ts` reran 29 `test-run`
tasks, 132 to 142 task-seconds, whether the edit was to a body, a type, a
comment or an export. Of the last 600 commits on `main`, 80 touch a library's
`src/` outside tests. In 5 of them, every touched file emits the same
JavaScript.

## Decision

1. **`build-emit` writes the emit manifest of the project's dependency
   closure, the project included.** The manifest has one entry per file that
   Git does not ignore, plus each member's `.gen` tree.
   - A TypeScript module (`.ts`, `.mts`, `.cts`, `.tsx`) is keyed on the
     SHA-256 of the JavaScript that `Bun.Transpiler` emits for it. The
     transpiler gets the compiler options of the tsconfig that governs the
     module, as Bun picks it: the nearest `tsconfig.json`, with its `extends`
     chain merged.
   - Any other file is keyed on its bytes: a tsconfig, a `package.json`, a
     `.sql` file, a JavaScript module.
   - A declaration file (`.d.ts`, `.d.mts`, `.d.cts`) gets no entry, because
     Bun never runs one.
   - The task's key reads the closure's files, its own files, the workspace's
     `bunfig.toml` and `tsconfig*.json`, and its `generate`.
2. **Every narrowing falls back to bytes.** A narrowing that cannot be shown
   to hold keys on bytes, which costs a rerun and never a stale verdict:
   - A module that does not transpile is keyed on its bytes.
   - So is a module whose tsconfig chain reaches a package (`extends:
     "@tsconfig/bun"`), or a config the task's key does not read. Such a
     config is anywhere other than a closure member or the workspace root.
   - When a `bunfig.toml` of the workspace or of a closure member declares a
     `preload`, every module of the manifest is keyed on its bytes. The same
     applies when a closure module names `Bun.plugin` or imports `plugin` from
     `bun`. In each case, code can transform modules before Bun runs them, so
     what runs is no longer only what the transpiler emits. The manifest
     records the mode and the reason.
3. **`test-run` folds artifacts, not its dependencies' keys.** Its key folds
   these digests through the artifact input of CLI ADR 0043:
   - the manifest of its project's `emit` step;
   - the `.gen` directory of its own `generate`;
   - the `outDir` of `^tsc` and `^tsc-auto`, which is empty for a package that
     Bun compiles.

   It keeps its own sources and tests as direct inputs. A change that leaves
   every emitted module and every other closure file byte-identical reruns no
   dependent test. A change that alters emitted JavaScript reruns every
   dependent it reaches, and so does a change to a file read at run time, to a
   config or to a manifest.
4. **Type-checking keeps the sources.** `test-types` and `build-tsc` still
   fold their dependencies' keys. A declaration a dependent type-checks
   against still reruns that type-check.

## Consequences

- **Measured gain.** After a type-only or comment-only edit in
  `@putnami/runtime`, `putnami test --tag ts` runs 1 `test-run` (runtime's own
  tests) instead of 29. `test-types` still reruns: 30 tasks, about 235
  task-seconds.
- **No declaration cutoff yet.** The declarations tsc emits for
  `@putnami/runtime` hold through a body-only or comment-only edit. Keying
  `test-types` on them would still need two things this decision does not
  give:
  - a fold of the declarations of the whole closure, not only of the direct
    dependencies, since a re-export carries a transitive type;
  - a verdict scoped to the project's own files, since a dependent's check
    also reports errors located in its dependencies' sources.
- **Source positions are not keyed.** The emitted JavaScript keeps no line or
  column, so an edit that shifts lines in a dependency does not rerun its
  dependents. Such an edit changes only the positions that a dependency's
  stack trace reports. A test whose verdict reads those positions is outside
  this rule.
- **Cost of `build-emit`.** One Bun process per project. It hashes every
  closure file and transpiles every closure module: on `@putnami/application`,
  1,789 files and 589 modules take 519 ms.
- **How another extension adopts this.** An extension whose tests execute
  transformed sources adopts the cut the same way:
  - it writes a manifest of what runs;
  - it folds that manifest as an artifact;
  - it keys on bytes wherever the transformation is not its own to reproduce.
