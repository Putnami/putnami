# ADR 0002 — A delegated stack keeps its build tool's cache

- **Status**: accepted
- **Date**: 2026-11-08
- **Scope**: `tooling/stacks` (`@putnami/stacks`), `tooling/pants`
  (`@putnami/pants`) and the delegated stacks that follow it (Bazel)

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
- Impact selection is Putnami's own, on the project graph the stack extension
  writes: a change runs its package and the packages that depend on it.

`@putnami/pants` is the first delegated stack. `putnami init --from pants`
declares it and synchronizes the projects; its sync task writes each
package's `putnami.json` with the edges of the `pants peek ::` export.

## Rejected alternatives

- **Import Pants targets as Putnami tasks.** Two caches over one build, and
  Putnami's key weaker than the tool's.
- **Keep Putnami's cache on, with the tool's inputs.** The key cannot cover
  what the tool resolves at run time, so a hit could replay a stale result.
- **Run `pants test <dir>::`.** The recursive spec runs a nested package's
  targets in its parent's task too, so one change runs them twice and
  attributes them to the wrong project; `<dir>:` names one directory's
  targets and the targets they generate.

## Consequences

- A delegated task always runs, and is as fast as the tool's own cache.
- A package without a test target has its `test` disabled in its
  `putnami.json`: a test task that ran nothing is no pass.
- The shipped Pants profile reads Python coverage; a repository without the
  Python backend overrides the test steps.
