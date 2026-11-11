# ADR 0004 — An @nx/js:tsc build moves when the tsc build reproduces its emit

- **Status**: accepted
- **Date**: 2026-11-11
- **Scope**: `integrations/nx` (`@putnami/nx`) import mode; builds on
  [ADR 0010 of `@putnami/typescript`](../../../../typescript/extension/doc/adr/0010-a-tsc-build-declares-its-outdir-through-pathfrom.md)

## Context

`@putnami/typescript` compiles a library with the workspace's `tsc`, into the
`outDir` its `tsconfig` names, where the packages that depend on it resolve
its `main` and `exports`. Import mode moves a `build` target whose
`package.json` script runs `tsc` alone to that build, and kept every
`@nx/js:tsc` target on Nx, because the executor does not emit what `tsc` on
the `tsconfig` emits:

- it requires `outputPath`, which it sets as `outDir`, and sets `rootDir` to
  its `rootDir` option, else the project root, so it writes
  `<outputPath>/src/index.js` where the `tsconfig` alone writes
  `<outDir>/index.js`;
- it writes a `package.json` into `outputPath` (`generatePackageJson`),
  copies `assets`, runs `transformers`, and puts a `.tsbuildinfo` the
  `tsconfig` does not place where it chooses;
- it reads its options from `targetDefaults` in `nx.json` and from the
  target's default configuration, not only from the target.

A library whose executor options say what its `tsconfig` says gains nothing
from Nx that Putnami's cache does not give, and stays on Nx all the same.

## Decision

- The sync maps an `@nx/js:tsc` `build` target when, its options merged as
  Nx merges them, the tsc build writes exactly what the executor writes. The
  target lies over the `targetDefaults` entry of its executor, else of its
  name, else of the first glob key that matches its name, and an entry with
  another executor applies to none; the target's executor,
  `defaultConfiguration`, options and each configuration's options win key
  by key; the options of the default configuration apply last; and
  `{projectRoot}`, `{workspaceRoot}` and `{projectName}` are replaced, a
  leading `{workspaceRoot}/` removed. Then:
  - no `assets`, no `transformers`, and `generatePackageJson` set to
    `false`; every other option is one the executor reads only to write the
    `package.json` it no longer writes, or `main`, `clean` and `watch`. Any
    other option keeps the target on Nx, among them `external` and
    `externalBuildTargets`, with which the executor inlines dependencies into
    its `outputPath` and rewrites their imports;
  - `tsConfig` names the config the tsc build compiles: the one the
    project's `build` script names, which must run `tsc` alone, else
    `tsconfig.lib.json`, else `tsconfig.json`;
  - `outputPath` is that config's `outDir`, inside the project, where the
    tsc build owns and restores its output (ADR 0010), and `rootDir`, else
    the project root, the `rootDir` the config declares; a config that
    declares none lets `tsc` compute it from the sources, which the sync
    does not;
  - the build writes no `.tsbuildinfo`, or the config names it with
    `tsBuildInfoFile`.
- For a mapped target the sync writes `options["@putnami/typescript"].compiler
  = "tsc"` in the project's `putnami.json` when the project sets none, since
  no `build` script selects the tsc build, records that in
  `options["@putnami/nx"].typescriptCompiler`, and removes only a value it
  wrote once the target no longer maps. A project that sets another
  compiler keeps the target on Nx, since the tsc build would not run.
- Every other `@nx/js:tsc` target keeps running through Nx, listed with the
  first condition it fails.
- The sync reads the `tsconfig` chain with the reader `@putnami/typescript`
  plans its build with, which moves to the extension SDK's `tscscript`
  package, so the two never disagree on an `outDir` or a `rootDir`.

## Rejected alternatives

- **The tsc build takes the executor's options**, an `outDir` and a
  `rootDir` passed to `tsc`, and writes the `package.json` and assets. It
  maps more targets, but a default `outputPath` lies at the workspace root,
  outside the project the build owns and restores (ADR 0010), and the
  generated `package.json` rewrites the dependencies the executor computes
  from Nx's graph: the build would grow a second Nx executor.
- **Rewriting the target's options** so they match the `tsconfig`. Import
  mode never rewrites `project.json` (the spec's non-goal), and a team that
  wants the move changes two options itself, which the listed reason names.

## Consequences

- A library whose `@nx/js:tsc` options repeat its `tsconfig` builds and
  restores through Putnami, at the paths Nx wrote, so its dependents resolve
  the same files.
- The reason a target stays names the option to change, so a team moves a
  library by editing its `project.json` or `tsconfig`, and the next sync
  maps it.
- Two differences remain outside a successful compile. The executor
  compiles with `noEmitOnError`, so a type error leaves its `outputPath`
  empty, where the tsc build emits and then fails the task. With `clean`
  `false` the executor keeps files a source no longer emits, which the tsc
  build removes.
- A change of the executor's behaviour in a later Nx release that reaches
  these options needs this mapping revisited.
