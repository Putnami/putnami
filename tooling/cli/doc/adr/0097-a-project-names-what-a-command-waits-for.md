# ADR 0097 — A project names what a command waits for

- **Status**: accepted.
- **Scope**: `protocols/workspace` (`ProjectConfig.Commands`), `@putnami/cli`
  (`internal/jobs/planner`, `internal/jobs/cachekey`, the scheduler's key
  publication), `@putnami/rush`, `@putnami/turbo`, `@putnami/moon`
- **Date**: 2026-11-14

## Context

A command can wait for another: an extension manifest declares
`dependsOn: ["build"]` for the project's own build, or `["^build"]` for the
build of every project it depends on. The manifest is the same for every
project and every adoption mode, so it cannot say what a monorepo tool says
per task. Three adoptions lost the order their tool kept:

- A delegated `rush test --only <package>` waited for no build, so on a fresh
  clone it read upstream outputs that did not exist yet.
- A `rushx build` kept on Rush in import mode, and a task moved to
  `@putnami/typescript`, waited only for the steps of their own extension, so
  they ran before, or beside, the upstream builds the tool would have run
  first: Turborepo's `dependsOn: ["^build"]`, moon's task `deps`, Rush's
  build order.
- A delegated task has its cache off and publishes no key. A task that waited
  for it folded nothing of it into its own key, so an edit to a dependency
  moon builds left a moved test's key unchanged, and a stored pass was
  replayed.

## Decision

- A project's `putnami.json` may name, under `commands.<command>.dependsOn`,
  the commands that command waits for in that project, in the manifest's own
  syntax restricted to two forms: `command` and `^command`. The planner adds
  them to that project's jobs of the command, and plans and orders them as it
  does a manifest's dependency. A project that runs no job of a prerequisite
  gets no edge.
- A cacheable task's key folds, for each direct dependency in another project
  that publishes no key because its cache is off, the key that dependency
  would have, which folds its own dependencies' in turn: the same computation
  an invocation-scoped producer already contributes to its consumer. For such
  a dependency in the task's own project, such as a build kept on moon behind
  `~:build`, it folds the digest of what reaches that dependency from other
  projects: the keys its dependencies publish and the would-be keys of its
  keyless dependencies elsewhere. The project's own files are left out: the
  key already reads them, and a keyless fixer that rewrites them would move
  the key between a run and the next. An identity addresses no entry. It is
  taken when its job starts, from the bytes it reads before it writes, or
  when a dependent first asks for a job that was pruned, as the plan-time
  keys take it before anything runs; the Rush and moon profiles exclude the
  tools' outputs and `node_modules` from a delegated task's inputs.
- Impact selection carries a project's `^command` entry into the dependent
  command of that project alone, as `impact-reads-only-what-a-project-runs`
  requires, and lets the prerequisite's tasks travel on through every
  dependent, as a manifest's do, since the entry reads the whole dependency
  closure. A same-project `command` entry links the two commands' tasks in
  every project, as a manifest's does.
- The adoption syncs write these entries from what the tool declares: Rush's
  build order and phased commands, `turbo.json`'s `dependsOn`, moon's
  resolved `deps`. A delegated task keeps one tool process on its own project
  (`delegate-runs-one-process-per-task`): `rush test --only` stays, and waits
  for the `rush build --to` the project's build task runs.

## Rejected alternatives

- **A static `^build` in a manifest.** It would make every TypeScript
  project's test wait for every upstream build in every workspace, and in
  Rush's delegate mode run `rush build --to` once per upstream project
  ([stacks ADR 0008](../../../stacks/doc/adr/0008-a-delegated-stack-keeps-its-tool-cache.md)).
- **`rush test --to <package>`.** It tests the upstream projects too, which
  the settled decision refuses.
- **Turning a delegated task's cache on to give it a key.** Putnami's key
  cannot cover what the tool resolves at run time (stacks ADR 0008).
- **`!command` and `command~step` from a project file.** No adoption needs
  them, and a session barrier from one project would order every project's
  run.

## Consequences

- `putnami test` on a project whose file names `build` plans that build too,
  as the tool's own test command would.
- The key of a task that waits for a delegated task changes once, when this
  lands, and afterwards whenever that task's inputs change.
- An older CLI ignores the member, with the leniency it has for any unknown
  project member, and plans without the order.
