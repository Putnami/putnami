# ADR 0008 — A delegated stack keeps its build tool's cache

- **Status**: accepted
- **Date**: 2026-11-08
- **Scope**: `tooling/stacks` (`@putnami/stacks`), `monorepo/pants`
  (`@putnami/pants`), `monorepo/nx` (`@putnami/nx`), `monorepo/moon`
  (`@putnami/moon`) and the delegated stacks that follow it (Bazel)

## Context

Pants and Bazel are build systems with their own hermetic cache and remote
execution. Importing their targets as Putnami tasks would duplicate that cache
and weaken it: Putnami's key would cover the files of one directory, where the
tool's key covers every transitive input it resolved. A team on Pants still
wants Putnami's runner, impact selection and verdict.

## Decision

A stack profile may set `"delegated": true`. A delegated stack runs its tool
on each affected component and reads its reports, and the tool's cache
decides what reruns:

- Every task of a delegated stack's manifest sets `cache.enabled` to `false`:
  Putnami stores no entry and replays none. The conformance kit holds the
  manifest to the profile.
- The task result records `delegatedTo`, the stack id, so the run's output
  says which tool produced the result.
- A task's `dir` may be `root`: its steps run in the stack root, where the
  tool resolves its addresses, instead of the project directory.
- Each command of a delegated stack runs a `graph` step, then its task. The
  `graph` step depends on the `graph` step of each project upstream and runs
  nothing; the task depends on its own project's `graph` step alone. Impact
  selection is Putnami's own, on the project graph the stack extension
  writes: a change runs the tool on its package and the packages that depend
  on it, and a package it depends on runs its `graph` step only.

`@putnami/pants` is the first delegated stack, and `@putnami/nx` the
second: its tasks run `nx run <project>:<target>` at the workspace root, and
Nx's `dependsOn` and cache, local or Nx Cloud, answer the upstream targets. `putnami init --from pants`
declares it and synchronizes the projects; its sync task writes each
package's `putnami.json` with the edges of the `pants peek ::` export.

## Rejected alternatives

- **Import Pants targets as Putnami tasks.** Two caches over one build, and
  Putnami's key weaker than the tool's.
- **Keep Putnami's cache on, with the tool's inputs.** The key cannot cover
  what the tool resolves at run time, so a hit could replay a stale result.
- **Make each task depend on the same task upstream (`^test`).** A
  dependency would run its tool on every change to a package that depends on
  it, with no Putnami cache to absorb it.
- **Run `pants test <dir>::`.** The recursive spec runs a nested package's
  targets in its parent's task too, so one change runs them twice and
  attributes them to the wrong project; `<dir>:` names one directory's
  targets and the targets they generate.

## Consequences

- A delegated task always runs, and is as fast as the tool's own cache.
- A package without a test target has its `test` disabled in its
  `putnami.json`: a test task that ran nothing is no pass.
- A `BUILD` directory that holds no package, such as one of third-party
  requirements, is a member with no extension that runs no task. The sync
  writes an edge from each package to the third-party directories it depends
  on, so a change to a requirement or a resolve lockfile runs those packages.
- The shipped Pants profile reads Python coverage; a repository without the
  Python backend overrides the test steps.
- moon is delegated the same way, and moon names a task per project: the
  sync writes the step of a task moon tags rather than names as an override
  in the project's `putnami.json`, and disables a command the project has no
  task for. Adopted in import mode instead, a moon project whose language has
  a Putnami extension runs that extension under Putnami's cache, and every
  other project stays delegated: no project runs both, so no Putnami cache
  sits over moon's.
