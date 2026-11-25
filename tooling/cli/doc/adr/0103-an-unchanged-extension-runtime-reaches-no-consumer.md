# ADR 0103 — An unchanged extension runtime reaches no consumer

- **Status**: accepted
- **Date**: 2026-11-25
- **Amends**: [ADR 0044](0044-selection-is-task-level.md), decision 2 (the
  extension-consumer edge), and the extension-consumer scope of
  [ADR 0042](0042-impacted-plans-from-the-commit-and-records-why.md)
- **Amended by**: [ADR 0104](0104-a-hosted-run-compares-runtimes-prepared-before-it.md),
  decision 4: `putnami extensions prepare` admits the runtimes a hosted run
  compares, and the hosted comparison no longer requires a provider
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
is a function of the code it links. Rebuilt at `2bc75a09` with a comment
appended to `protocols/cache/cache.go` and a line added to one of its tests, the binary
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
- **A request a runner provider executes.** A frozen request runs on the
  runner's platform, where a runtime this machine proves unchanged may differ,
  for example through a file only that platform compiles; its selection
  compares nothing.
- **Anything not proven.** The selection compares only what it can: an
  extension whose runtime it could not prepare, whose base runtime is missing,
  or whose base checkout failed keeps its edge. Selection shrinks on a proof
  only.

### 3. How the selection proves it

`--impacted` walks once. For every extension whose runtime fired an
extension-consumer edge in that walk and that holds no changed file of its own
(`ImpactTrace.RuntimeEdges` minus the seeds), `CompareExtensionRuntimes`
writes the files of the diff's base commit as raw blobs into a temporary
directory (`git.WriteFilesAtCommit`: no hook, filter or repository code runs),
prepares, or loads, the runtime of this tree and the runtime of that copy, both
in the machine's artifact store under the digest of their own inputs, and
compares their contents. The base description resolves its prepare toolchains
against the base's lock, and two runtimes whose locked prepare toolchains
differ never compare unchanged. The walk runs again with the extensions proven
unchanged (`ChangeImpactOptions.UnchangedRuntimes`). The selection prints a
line naming each extension it cut off and each it kept with the reason, and
the impact trace record carries the same answers.

### 4. A hosted run looks both runtimes up and prepares nothing

A run that holds the run credential executes no repository code during the
run ([ADR 0055](0055-run-credentials-stay-out-of-repository-processes.md)): it
starts only the runtimes the source build prepared before it. The comparison
follows the same rule. It compares only extensions the source build prepares,
looks the head's runtime and the base's runtime up in the artifact store by the
digests of their inputs, and prepares neither. A runtime the store does not
hold keeps the edge, and the explanation says so. Preparing base runtimes
before a hosted run, as `./putnamiw --print-engine` prepares the head's, is
left to a later decision.

## Consequences

- A change that leaves the linked code and its positions alone no longer
  re-runs that extension's tasks across the workspace: a test, a document
  inside a Go module, code in a package the extension does not link, or a
  comment that shifts no linked line. A comment inserted above linked code
  moves the line table the binary records, and fires the edge as before.
  Measured on this change with `lint,test,build,validate --impacted
  --dry-run`: a comment appended to `protocols/cache/cache.go` plans 94
  projects, 585 tasks and 9 test suites instead of 159, 2,048 and 129; a
  changed error message in `tooling/extension-sdk/filelock` still plans 160
  projects, 2,235 tasks and 148 suites.
- A local `--impacted` run that reaches an extension writes the base tree once
  and prepares each reached extension's base runtime once per base; the store
  keeps it for every later run against that base. Measured on this repository
  with 28 compared extensions: about 10 seconds with a warm artifact store,
  about 50 with an empty one and a warm Go build cache.
- ADR 0104 lets a hosted run of a workspace that builds its own CLI answer
  equal locked digests unchanged, and otherwise compare the runtimes
  `putnami extensions prepare` admitted before the run; elsewhere a hosted run
  compares nothing.
- `projects why` and `why_impacted` answer a change to a project as a whole,
  with no diff to compare runtimes against, so they still name the
  extension-consumer edges this decision cuts off on an actual diff.
- The cache key is unchanged: a consumer task that does run keys on the head
  runtime, as before.
