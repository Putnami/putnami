# ADR 0001 — The adoption mode is a workspace option the sync reads

- **Status**: accepted
- **Date**: 2026-11-09
- **Scope**: `monorepo/nx` (`@putnami/nx`) and `tooling/cli`
  (`putnami init --from`); builds on
  [ADR 0008 of `tooling/stacks`](../../../../tooling/stacks/doc/adr/0008-a-delegated-stack-keeps-its-tool-cache.md)
  and D-064

## Context

`putnami init --from nx` adopts a Nx repository in one of two modes (D-064).
In delegate mode every build, test and lint target runs through `nx run`,
and Nx keeps its cache. In import mode Putnami's runner and cache take over
the targets it can map. The CLI must not read Nx's configuration: the
extension owns it. The mode must also survive the first `init`, because
every later `putnami projects sync` rewrites the projects' `putnami.json`.

## Decision

- `init` records the mode in the workspace options,
  `options["@putnami/nx"].mode`, `import` or `delegate`. The extension's
  sync task reads it, `import` when absent. A `mode` parameter of the sync
  job outranks it, so a rerun previews another mode without writing it.
- Both modes write one `putnami.json` per Nx project: the extension, the
  dependencies, and in `disable.jobs` the build, test or lint task the
  project has no target for.
- In import mode, `init` also declares `@putnami/typescript`, whose probe
  claims each project with a `package.json`. For a TypeScript project, one
  with `package.json` and `tsconfig.json`, the sync disables the qualified
  job of the extension that does not run a target:
  `@putnami/nx:<task>` when the TypeScript extension covers the target's
  executor or script, `@putnami/typescript:<task>` otherwise. One extension
  runs each task, so no Putnami cache sits over Nx's.
- A rerun of `init` without `--apply` runs the workspace sync tasks with
  `dryRun`, prints what they would change, and writes nothing.

## Rejected alternatives

- **A Putnami task per Nx target in import mode.** Putnami would run an Nx
  executor it cannot key, under a cache weaker than Nx's.
- **The mode in each project's `putnami.json`.** One repository has one
  mode; per-project modes would let a sync mix them silently.
- **Snapshot and restore the files on a rerun.** The sync tasks of other
  extensions write files the snapshot would not know; their own dry run
  already reports what they would change.

## Consequences

- Switching mode is `putnami init --from nx --mode <mode> --apply`.
- A build, test or lint target the TypeScript extension does not cover
  keeps running through Nx in import mode, and the sync lists it.
