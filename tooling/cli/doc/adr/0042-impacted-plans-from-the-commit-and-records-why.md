# ADR 0042 — `--impacted` plans from the commit, scopes an extension's consumers to its tasks, and records why

- **Status**: accepted
- **Scope**: `@putnami/cli` (`putnami install` lock write, `--impacted`
  selection and trace, `impacted` / `why_impacted` MCP tools),
  `@putnami/cli-model` (`workspace` extension-consumer edge)
- **Amended**: 2026-10-23, §3: the edge carries every task of the extension
  when its runtime moved and names the runtime input; a change confined to
  the task and step declarations of the extension's own manifest carries the
  steps it moved and names the manifest.

## Context

On a hosted runner, a two-project diff selected 479 tasks across 71 projects
where a local `--impacted` for the same merge base selected 48. Three causes
compounded:

- The runner's `putnami install` appended its own platform digest and download
  URL to `putnami.lock.json`, so the working-tree diff read the lock as a
  change and selected every project that declares it as an input.
- The extension-consumer edge selected every consumer of an in-workspace
  extension whole, so a Go tool change ran a workload's TypeScript and Python
  jobs too.
- The selection trace was a capped `--verbose` notice that no record kept.

## Decision

### 1. An install writes the lock only when a requested fact changed

A new or removed entry, a moved version or a migrated format writes the lock,
with this host's newly verified digests. A same-version entry whose only
differences are an unlisted per-platform digest and this host's download URL
is a machine-local refresh: the committed file stays byte for byte. The host
still verified its archive against the registry's digest; the next lock write,
or `putnami extensions update`, records it. The implicit first-use bootstrap
writes nothing.

This makes a hosted plan equal the local plan for the same commit and
baseline without any runner opt-in.

### 2. An explicit install names the committed files it rewrote

`putnami install` snapshots the tracked files that differ from `HEAD` before
its first phase and after its last, and warns with every committed file it
rewrote, on every host. It warns and does not fail, CI included: the hosted
runner rewrites committed files around the install by design (it swaps
`bun.lock` registry URLs for its dependency broker and restores them at exit),
and the CLI cannot tell that rewrite from one it should refuse. A hard guard
belongs to the runner, which knows its own rewrites.

### 3. A tool change re-runs the actions that use the tool

This is Bazel's rule: a changed toolchain invalidates the actions that invoke
it, not the packages next to them.

- The extension-consumer edge fires from an in-workspace extension whose
  runtime the change moved: the extension is fully impacted, or the tasks
  that read the changed files reach a dependent's task across a `^`
  reference ([ADR 0044](0044-selection-is-task-level.md)). It carries every
  task the extension declares, and the trace names the runtime input, the
  changed file the walk traced the runtime to: `runtime input
  tooling/extension-sdk/registrycred/install.go` for a module the
  extension's `go.mod` replaces. One implementation byte re-runs every task of
  the extension on every consumer; a `_test.go` of the extension re-runs only
  its own tests.
- A change to the extension's own `putnami.extension.json` moves no runtime
  when it edits only task and step declarations. The selection reads the
  manifest the diff's base records, compares each task's contract digest
  (`TaskContractDigest`) on both sides, and the edge carries the steps whose
  declaration moved: a step added, removed or edited, a step whose task's
  digest differs or whose task exists on one side only, the steps a
  dependent's `^` reference reaches from them, and the steps that depend on
  those inside a project. The trace names `task declaration
  <manifest>`. A change that moves no step carries nothing, and the edge does
  not fire.
- Any other manifest difference keeps the extension fully impacted, and its
  consumers run every task: hooks, the runtime block, a command's own
  members, the vocabulary, the version. So does a base that records no
  manifest or records one that does not parse, and a pinned build that
  replaces the manifest. When a runtime change and a confined manifest change
  of one extension reach the same consumer, the consumer runs every task and
  the trace names the runtime input.
- The edge selects each consumer for the tasks it carries, of that extension
  only. The scope entry is qualified by the extension project's id, because
  two extensions routinely declare the same `<command>~<step>` (ADR 0044 owns
  the spelling and the plan narrowing).
- `impacted` and `why_impacted` read the one propagation `--impacted` runs,
  so they cannot disagree on a project or its scope. `why_impacted` runs it
  from the one project it is asked about, with no changed file, so its
  consumer edges name no runtime input and carry every task.
- A portable execution request freezes the task scopes with the projects, so
  the executing engine plans the graph the submitter proved.

**The named approximation.** Nothing propagates out of a task-scoped
consumer: its own sources did not change, so its dependents are not selected
and, if it is an extension, its consumers are not either. A job of another
extension that reads what the scoped job rebuilt (a TypeScript job reading a
Go-generated client) is not re-run by this selection. Its cache key includes
that output, so the next run that plans it re-executes it instead of serving
a stale verdict.

### 4. The trace is a session record

A run that executes an `--impacted` selection records a `selection:impacted`
session event right after the opening `scheduler:parallel` event: baseline,
resolution tier, merge-base commit, every changed file, the uncommitted ones
(`uncommittedFiles`), every seed and edge, and every task-scoped project with
its tasks, uncapped. It rides the existing `/workspace:session-events`
identity, so it reaches the live JSONL stream and the session's
`events.jsonl` without a new record variant.

## Rejected alternatives

- **A committed-only `--impacted` mode the runner passes, failing on a dirty
  tree after bootstrap**: it needs a runner change to take effect and fails
  every hosted run whose lock lists one platform.
- **Failing `putnami install` on a committed-file rewrite**: it failed every
  hosted bootstrap, because the runner's own `bun.lock` rewrite is
  indistinguishable from a bad one.

## Consequences

- A change to an in-workspace extension's runtime runs, on every project
  under it, that extension's jobs and their predecessors, and nothing else. A
  change confined to its task and step declarations runs the jobs of the
  steps it moved and their predecessors. Consumer cache keys still include
  the extension's implementation digest and each task's contract digest, so
  a job the selection leaves out keeps a key its inputs still match.
- A lock recorded on one platform stays one-platform through a CI install
  until the next requested change or `putnami extensions update`.
- A path in `uncommittedFiles` on a CI runner is a file the run wrote before
  it planned: it names a bootstrap write this decision does not cover.
