# ADR 0005 — A named target runs as the task the workspace maps it to

- **Status**: accepted
- **Date**: 2026-11-13
- **Scope**: `integrations/nx` (`@putnami/nx`)

## Context

A Nx project often names its targets by variant: `test:lib`, `test:eslint`,
`lint:types`. The sync maps the targets named `build`, `test` and `lint`, so
on such a repository it disables `test` and `lint` on every project, and
`putnami lint,test --impacted` runs nothing. Nx states no relation between a
target's name and what it checks: only the team knows that `test:eslint`
lints.

The moon sync has the same case and solves it from moon's own data, a task
tagged with the command. It writes the step that runs the task in the
project's `putnami.json`, under the project's override of the profile
(`stacks.StepsOverride`).

## Decision

- `options["@putnami/nx"]` in `putnami.workspace.json` takes `targets`, an
  object that maps a Nx target name to `build`, `test` or `lint`:
  `{"test:lib": "test", "test:eslint": "lint"}`.
- A task of a project runs the target named after it, as before. A project
  with no such target runs each target mapped to the task, in name order.
  The sync leaves the task out of `disable.jobs` and writes, in the
  project's `putnami.json`, `options["@putnami/nx"].tasks.<task>.steps`: one
  `npx --no nx run {component}:<target> --outputStyle=static` per target,
  the profile's own step with the target's name.
- A mapped target runs through `nx run` in both modes. In import mode,
  `@putnami/typescript:<task>` is off for the project, as for any target
  that stays on Nx.
- The sync owns steps of the shape it writes: it replaces and removes them,
  and never writes over steps of another shape, which are the project's own.
- A mapped target that runs as its task is no longer listed. One on a
  project that has the task's own target is listed with that reason. A
  target with no mapping keeps its summary line.
- An entry no project has is reported as `nx.target_unmatched`, a warning,
  by the sync and by `validate`. A value that is no task, or a key named
  `build`, `test` or `lint`, fails the sync and `validate`, and writes
  nothing.
- `putnami validate` needs no new check: it compares each `putnami.json`
  with what the sync writes for the recorded mapping, and fails with
  `nx.sync_stale`.

## Rejected alternatives

- **A mapped target that moves to `@putnami/typescript` in import mode.**
  The import rules read a target by its name: the ESLint options of the
  `lint` target, the `build` script, the `targetDefaults` entry. Each rule
  would need its proof that the tool does not change
  ([ADR 0017](../../../../tooling/stacks/doc/adr/0017-an-import-never-changes-the-tool-that-checks-the-code.md)
  of `tooling/stacks`). A team that wants the move names the target after
  its task in Nx.
- **A mapping in each project's `putnami.json`.** The sync owns those files,
  and a repository names its targets the same way across projects: one
  entry answers for all of them, in the file a reviewer reads.
- **A guess from the name's prefix.** `test:eslint` lints, and `test:build`
  builds: the prefix says nothing reliable.
- **Two targets mapped to one task fail the sync.** A repository with two
  linters maps both to `lint`; the task runs one after the other.
- **A mapped target that runs beside the task's own target.** In import
  mode the task's own target may move to `@putnami/typescript`, and one task
  would then run in two extensions.

## Consequences

- The mapping is one workspace option, and a project leaves it by an edit
  of `putnami.workspace.json`, then `putnami projects sync`.
- A project's hand-written steps of the sync's shape are replaced at the
  next sync; steps with another argument are kept.
- Nx still decides what a mapped target runs and caches it. Putnami selects
  the projects and reads the exit code, and for `test` the reports the
  project's test configuration names.
- A target maps to no other task than `build`, `test` and `lint`.
