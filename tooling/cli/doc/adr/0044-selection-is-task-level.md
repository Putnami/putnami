# ADR 0044 — Selection is task-level

- **Status**: accepted;
  [ADR 0070](0070-a-task-scope-keeps-a-job-by-its-declared-side-effects.md)
  (proposed) supersedes the `publish` and `deploy` part of decision 5
- **Scope**: `@putnami/cli-model` (`workspace.TraceChangeImpact`,
  `TaskImpactIndex`), `@putnami/cli` (`internal/workspace.NewTaskIndex`,
  `internal/engine` plan narrowing)
- **Amended**: 2026-10-23, §3: an in-workspace extension's own manifest
  keeps the project full unless the index confines its change to task and
  step declarations; a confined change runs the steps it moved and the tasks
  that read the manifest.
- **Amended**: 2026-10-23, §5: a kept job keeps the jobs it depends on and
  no job it is only serialized after, the narrowed plan orders any two kept
  jobs as the full plan does, and a kept `validate` job keeps no `test` job
  the scope does not name.

## Context

A selection that maps a changed file onto its project and propagates over
project edges over-selects in three ways. A `.md` no action reads selects its
project and every importer. A `_test.go` selects every dependent whole,
although no dependent's action reads it. A dependency edge runs the
dependent's `lint`, which reads only the dependent's own sources. One line in
`go/framework/http/http_test.go` or its `README.md` planned 413 jobs across 15
projects.

A task already declares the files its action reads, for its cache key
([ADR 0043](0043-a-cross-project-carrier-task-keys-only-on-what-it-reads.md)).
Selection reads the same declarations.

## Decision

### 1. A changed file seeds the tasks whose declared inputs read it

The declarations are the ones the cache keys on, read through the same
protocol helpers:

- a task's `inputs` ports (`from: project`, `workspace` and `closure` file
  patterns) through `DeriveTaskCacheKey`;
- a project's `options.<layer>.filePatterns`, attributed to that layer's
  tasks;
- `options.generate.assets`, which every job of the declaring project keys on.

A file no declaration selects seeds nothing: its project is not selected, and
nothing downstream is. A pattern above the project root is matched in the
`../` form `filepath.Rel` produces, the coordinate system of the cache hasher.

### 2. Propagation follows the `^` references a task declares

`^generate` in a step of command `build` names the `build~generate` of every
project this one depends on. The impact walk reads that relation in reverse.
A task nothing references (`test`, `lint`) reaches no dependent. A reached
task carries, transitively, the tasks that depend on it inside the dependent,
because the key fold does the same. The contract edge carries tasks by the
same rule, with the provider in place of a declared dependency, when it fires
([ADR 0054](0054-the-contract-edge-fires-on-the-committed-contract.md)).

### 3. A claim the model cannot attribute keeps the project full

These claims are not task declarations:

- a scope's `putnami.json`, which decides which tasks exist;
- a caller's own workspace-input pattern (watch);
- a workspace-root entry a provider claimed through `watchedFiles` that no
  `from: "workspace"` input declares;
- a file of an in-workspace extension project that no task reads, because
  it may be the runtime its consumers execute;
- the extension's own `putnami.extension.json`, unless the index confines its
  change (`TaskImpactIndex.TasksOfManifestChange`). The index compares the
  manifest the diff's base records with the working tree's, and confines a
  change that differs only in task and step declarations to the steps it
  moved. The extension then runs those steps beside the tasks that read the
  manifest, and its consumers run those steps alone
  ([ADR 0042](0042-impacted-plans-from-the-commit-and-records-why.md) §3).
  The tasks that read the manifest propagate as any reader does, and do not
  count as a runtime change.

A task wrongly left out is a gate that never ran; one wrongly left in is a
cache hit. The index errs wide.

### 4. The model stays free of manifests

`TraceChangeImpact` is the one change→project pass, shared by `--impacted`
and watch. It takes a `TaskImpactIndex` built in the CLI from the loaded
manifests (`internal/workspace.NewTaskIndex`). A nil index reproduces the
project-level walk, so a caller that cannot load extensions still selects
something. `impacted` and `why_impacted` name the same scopes as the run.

### 5. Spelling and plan narrowing

- A task scope is `[<extension-project-id>#]<command>~<step>`, derived from a
  job's command and step, never from its display name. The qualifier appears
  only on an extension-consumer scope: `build~generate` exists in both Go and
  TypeScript, and an unqualified entry would keep a consumer's TypeScript
  jobs for a Go tool change. The narrowing matches both halves.
- A project reached twice runs the union. Full dominates, and an upgrade
  re-propagates.
- `narrowToTaskScopes` keeps a job when its project is not task-scoped, when
  the scope names its task, or when a kept job depends on it, whichever
  extension owns it. The plan stays a closed DAG.
- A write-serialization edge (`SerializeAfter`) orders two jobs and keeps
  neither. A kept job's edge to a job the narrowing drops becomes edges to the
  kept jobs that the dropped job waits for, found through dropped jobs only.
  An edge that duplicates a functional dependency is left out. For any two
  kept jobs, the narrowed plan orders them as the full plan does.
- A kept `validate` job keeps no `test` job the scope does not name. The spec
  gate reads that job's report from its cache entry, and a check no entry
  answers warns ([ADR 0026](0026-spec-gate-evidence-scope.md) §5).
- `publish`, `deploy`, `validate` and `validate-workspace` are never narrowed.
  The release-set coordinator decides release members, and the validate steps
  read session reports rather than files.
- A finalizer is in the scope of what it finalizes, so a scope that keeps a
  test environment's setup also keeps its teardown.

## Consequences

- One line in `http_test.go` plans 228 jobs across 2 projects instead of 413
  across 15; the `README.md` plans 172 across 1. `lint` on a non-test source
  change falls from 30 planned jobs to 2.
- A `_test.go` re-runs its project's `test` and every task that also declares
  test files. In Go that includes `lint-golangci-fix`, which reads them.
  Narrowing that is a manifest change.
- A README still selects `@putnami/cli-documents`, which declares `git:**` as
  a `test` input: it is the documentation drift gate.
- Nothing propagates out of the tasks an extension-consumer edge added
  ([ADR 0042](0042-impacted-plans-from-the-commit-and-records-why.md)).
