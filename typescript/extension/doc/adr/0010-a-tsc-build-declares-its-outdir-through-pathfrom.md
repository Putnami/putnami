# ADR 0010 — A tsc build declares its outDir through pathFrom

- **Status**: accepted
- **Scope**: `@putnami/typescript` (`typescript/extension`), the `build-tsc`
  task and the `compiler` parameter of `build`, `test` and `package`;
  `@putnami/nx` and `@putnami/turbo`, whose import maps a `tsc` build

## Context

`putnami build` transpiles a library with `bun build` into
`.putnami/out/<project>/build`. A Node monorepo builds most of its libraries
with `tsc`, and the packages that depend on one resolve its `main` and
`exports` in the `outDir` its `tsconfig` names, inside the library. So
`putnami init --from nx|turbo --mode import` left every `tsc` build on the
monorepo tool, with no Putnami cache.

A task declares its filesystem outputs in the extension manifest. A literal
`path` is the same for every project, and the `outDir` is not: `dist` in one
library, `lib` or `build` in the next. The issue that asked for the change
left open whether the build declares that directory or links it to
the command output.

Two facts constrain the answer:

- The packages that depend on the library read the `outDir` itself, so a
  cache hit must put the bytes there, not a link to them.
- A Putnami restore links each file to its blob in the store, so the file
  keeps the time the blob was stored. On a fresh checkout it is older than
  the sources. `tsc -b` in a package that references the library then finds
  the library out of date, rebuilds it and rewrites its `.tsbuildinfo`: a
  write into another project. Observed with TypeScript 6.0.2; `tsc -p` on the
  same tree compiles the package alone and writes nothing in the library.

## Decision

1. **The compiler is a parameter.** `build`, `test` and `package` take
   `compiler`: `auto` (the default), `bun` or `tsc`, set in
   `options["@putnami/typescript"].compiler` or with `--compiler`. Under
   `auto`, a project compiles with `tsc` when the `build` script of its
   `package.json` runs `tsc` alone; the
   `tscscript` package (`tooling/extension-sdk/tscscript`) of the
   extension SDK states that rule once, for this extension and for the Nx and
   Turborepo imports.
2. **`build-tsc` declares its outputs with `pathFrom`.** The task resolves the
   config's `outDir`, `declarationDir` and `.tsbuildinfo` and reports each
   through a data port. Each is a `pathFrom` output under the `project` root,
   `optionalEmpty` because an `auto` run that does not compile reports none.
   The `outDir` and `declarationDir` carry `readByDependents`, so every job of
   a dependent project runs after the task.
3. **The task empties its outputs itself.** The engine never empties a
   `pathFrom` output, so `build-tsc` removes them before `tsc` starts, after
   it has checked that Git ignores them and tracks no file in them.
4. **A config with references runs as `tsc -p`.** The task runs the build
   script's command with its arguments, except `tsc -b` on a config that
   declares `references`, which runs as `tsc -p` on the same config. Putnami
   builds or restores each reference first, in dependency order: the `^tsc`
   edge of the step and `readByDependents` order it, and `^tsc` folds the
   reference's key into the dependent's.
5. **The key reads what `tsc` reads.** The project's files that Git does not
   ignore (`git:` patterns, so the emitted output never keys its own build),
   the top-level JSON and `tsconfig` files of each project of the dependency
   closure (a shared configuration package, a reference's config), the root
   `tsconfig` files and lockfiles, and the upstream `tsc` keys.
6. **`test` plans the same step**, so a test-only run of a dependent compiles
   or restores its libraries and keys on them. **`package` refuses** a `tsc`
   project: it would pack a tarball without the compiled code.
7. **Bun runs `tsc`.** The task starts the compiler of the workspace's
   `typescript` package, the first `node_modules/typescript/bin/tsc` from the
   project up to the workspace root, with the task's Bun, never a `node` from
   `PATH` and never a download, as the `no-node-on-the-host` requirement
   says. It does not start `node_modules/.bin/tsc`, which pnpm writes as a
   shell wrapper that starts `node`.

## Consequences

- A `tsc` library gets Putnami's cache: deleting its `outDir` and building
  again restores it, and its dependents build after it.
- The planner counts a `pathFrom` write without `within` as a write to the
  whole project root, so it records no precomputed key for the project's
  other tasks. That cost applies only to projects that compile with `tsc`;
  `build-generate`'s `client` output already pays it.
- The step is spliced out at plan time for an `auto` project whose
  `package.json` does not contain `tsc`, so the plan and the keys of a Bun
  project keep their shape. A project whose `package.json` merely mentions
  `tsc` (a `typecheck` script, an `@tsconfig/*` package) plans a step that
  compiles nothing, and its dependents wait for it.
- A cold `test,build` run compiles a `tsc` library once per command: the two
  steps are separate keys, as `build~generate` and `test~generate` are.
- A `tsc -b` script on a config with references does not run as written. Its
  emit is the same, and the build order of the references moves to Putnami.
- An `outDir` that Git does not ignore, or that holds a tracked file, fails
  the build and names the setting.

## Alternatives

- **Emit into `.putnami/out` and link or copy into the `outDir`.** The link
  or copy is a write to the project that no output declares, and a restore
  brings back the link, not the bytes. `readByDependents` is also invalid on
  the `command-output` root.
- **A literal path per project.** It needs a protocol change and a CLI that
  reads `tsconfig` and its `extends` chain at plan time, or a copy of the
  `outDir` in `putnami.json` that drifts from the `tsconfig`.
- **Force `--outDir` to a fixed path.** It breaks the `main` and `exports`
  the dependents resolve.
- **Keep `tsc -b` and fail when it would rebuild a reference.** It fails on
  nearly every fresh checkout that restores the references.
- **Restore with fresh timestamps.** It needs a copy instead of a link for
  every restored file, a change of the CLI's store outside this decision.
- **An input selector that lists `tsc`'s own root files.** It is more precise,
  but it needs a second reading of `include` and `exclude` that must agree
  with `tsc`, and `cliContract` 14. The `git:` candidates already keep
  ignored and emitted files out of the key.

## Contracts affected

- The `@putnami/typescript` manifest: the `compiler` flag, the `build-tsc`
  task and its `tsc` and `tsc-auto` steps in `build` and `test`, and `^tsc`
  edges. No CLI or protocol contract changes.
- `@putnami/nx` and `@putnami/turbo` map a `tsc` build to
  `@putnami/typescript:build`, and Nx writes the `compiler` it needs.

## Migration and reversal

Adding the flag moves the key of every `build`, `test` and `package` task
once, as an extension release already does. Reverting removes the step and
the flag: a `tsc` project then builds with Bun, or stays on the monorepo
tool after the next sync.
