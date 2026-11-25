# ADR 0103 — An unchanged extension runtime reaches no consumer

- **Status**: accepted
- **Date**: 2026-11-25
- **Amends**: [ADR 0044](0044-selection-is-task-level.md), decision 2 (the
  extension-consumer edge), and the extension-consumer scope of
  [ADR 0042](0042-impacted-plans-from-the-commit-and-records-why.md)
- **Scope**: `@putnami/cli-model` (`workspace.TraceChangeImpact`,
  `ChangeImpactOptions.UnchangedRuntimes`), `@putnami/cli`
  (`internal/workspace` impacted selection, `internal/jobs/scheduler`
  `CompareExtensionRuntimes`, `internal/engine` selection), features
  `cli/workspace-model` and `cli/workspace-discovery-selection`

## Context

The extension-consumer edge fires from an in-workspace extension whose
runtime the change moved, and hands every consumer every task that extension
declares. "Moved" is read from the inputs: a changed file anywhere in the
extension's dependency closure. In this repository 103 of the 200 projects are
in the runtime closure of at least one workspace extension, so a change to
any of them re-runs the extension's tasks on every project it runs in.

Measured on 2026-11-24 with `lint,test,build,validate --impacted --dry-run`
at `5887a75f`: one line in `protocols/cache` (5 dependents in the graph)
planned 159 projects, 2,048 tasks and 129 `test~test` suites, because
`/go/extension` imports it; one line in a generated cloud client planned
159 projects and 1,071 tasks for 4 real test runs, because
`/cloud/extension` imports every client.

The inputs over-approximate what consumers execute. `/go/extension` prepares
its runtime with `-trimpath -buildvcs=false` and `CGO_ENABLED=0`, so its binary
is a function of the code it links. Rebuilt at `2bc75a09` with a comment added
to `protocols/cache/cache.go` and a line added to one of its tests, the binary
is byte for byte the same; so it is with a new function in
`tooling/extension-sdk/npmpublish`, a package it does not link. A change to an
error message in `tooling/extension-sdk/filelock`, which every extension
links, changes it.

## Decision

### 1. The edge fires on a runtime that differs from the base's

An in-workspace extension whose runtime, prepared from this tree, holds the
same files with the same bytes and executable bits as the runtime the diff's
base prepares fires no extension-consumer edge for a runtime input. Its
consumers are not selected for its tasks. The extension itself keeps the tasks
its dependency edges carry: its own tests still read the change.

### 2. What the cut-off never applies to

- **A seed.** A changed file of the extension project itself keeps the edge:
  its root holds files its tasks read at run time beside what it prepares,
  and its manifest decides what runs.
- **A confined manifest change** (ADR 0044 §3) fires as before: it names the
  steps whose declaration moved, which no runtime comparison speaks to.
- **Anything not proven.** The selection compares only what it can: an
  extension whose runtime it could not prepare, whose base runtime is missing,
  or whose base checkout failed keeps its edge. Selection shrinks on a proof
  only.

### 3. How the selection proves it

`--impacted` walks once. For every extension project the walk reached without
a changed file of its own, `CompareExtensionRuntimes` prepares, or loads, the
runtime of this tree and the runtime of a detached worktree of the diff's base,
both in the machine's artifact store, under the digest of their own inputs,
and compares their contents. The walk runs again with the extensions proven
unchanged (`ChangeImpactOptions.UnchangedRuntimes`). The selection's
explanation names each extension it cut off, and each one it could not.

### 4. A hosted run looks the base runtime up and prepares nothing

A run that holds the run credential executes no repository code during the
run ([ADR 0055](0055-run-credentials-stay-out-of-repository-processes.md)): it
loads, never prepares, the runtimes it starts. It reads the base checkout as
git objects and looks the base runtime up in the artifact store by the digest
of the base's inputs. A base runtime the store does not hold keeps the edge,
and the explanation says so. Preparing base runtimes before a hosted run, as
`./putnamiw --print-engine` prepares the head's, is left to a later decision.

## Consequences

- A change that moves no linked code, such as a test, a comment, a document
  inside a Go module, or code in a package an extension does not link, no
  longer re-runs that extension's tasks across the workspace. Measured on
  `2bc75a09` with this change: the `protocols/cache` comment plans 94
  projects, 585 tasks and 9 test suites instead of 159, 2,048 and 129; the
  `filelock` message still plans 160 projects, 2,235 tasks and 148 suites.
- A local `--impacted` run that reaches an extension prepares its base runtime
  once per base: one detached worktree and one preparation per reached
  extension, cached in the store for every later run against that base.
- Until base runtimes are prepared before hosted runs, a hosted run cuts off
  only the extensions whose base runtime its store already holds.
- The cache key is unchanged: a consumer task that does run keys on the head
  runtime, as before.
