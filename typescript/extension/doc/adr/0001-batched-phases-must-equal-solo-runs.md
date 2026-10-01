# ADR 0001 — A batched phase must equal the solo runs it replaces

- **Status**: accepted
- **Scope**: `@putnami/typescript` (`typescript/extension`)

## Context

Bun and `tsc` pay a large fixed startup cost. Running them once per project
spends most of a workspace build starting processes, so the extension batches
many projects into one invocation.

A batch can silently diverge from the solo runs in three ways. Shared output
state: two projects sharing one `tsc` build-info directory produce declarations
that depend on which ran first. Shared failure: one compile error aborts the
invocation and leaves every sibling without output. Shared identity: an artifact
written to a path derived from the invocation lands in the wrong project, and
the cache restores it there.

## Decision

Batching is an execution strategy, never a semantic one. Every batched phase
(generate, transpile, types, compile) writes each project's artifacts, including
the type-checker's build-info state, into that project's own output directory.
It attributes each diagnostic and each failure to the project that produced it.

Equality with the solo run is tested: each batched phase carries tests that run
the batch and the solo path over the same inputs and compare the artifacts.

A failure isolates. The failing project reports `FAILED`, its siblings still
produce their outputs, and a failure with no parseable per-project output still
emits a diagnostic, never an empty non-zero result.

Batch membership is a declared property of the task, so the scheduler, not the
job, decides what may be grouped.

## Invariants

- No two projects in a batch share an output directory or an incremental state
  directory.
- A batched phase's per-project artifacts equal the solo phase's.
- A failing project does not suppress a sibling's output.
- Every failure path emits at least one diagnostic.

## Rejected alternatives

- **Run every phase per project.** Correct, and most of the build is process
  startup.
- **Accept "close enough" batch output.** Artifacts are cache payloads and
  published tarballs; one that varies with scheduling cannot be cached.
- **Abort the whole batch on the first failure.** A workspace-wide red hides
  which project broke.
- **Derive per-project paths from the invocation.** A restored cache entry then
  lands in the wrong project.

## Consequences

- A new build phase needs a batch boundary and a batch-equals-solo test before
  it can be batched.
- A phase that needs an invocation-scoped tool flag stays solo rather than being
  batched approximately.
- The report parsers that attribute tool output back to projects are part of
  this contract.
