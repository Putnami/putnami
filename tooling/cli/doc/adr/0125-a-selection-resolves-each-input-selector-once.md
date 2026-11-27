# ADR 0125 — A selection resolves each input selector once

- **Status**: accepted
- **Date**: 2026-11-27
- **Scope**: `@putnami/cli` (`internal/jobs/inputresolve`: the matcher change
  impact reads)
- **Related**: [ADR 0044](0044-selection-is-task-level.md)
  (the task index of an impacted selection), `protocols/workspace`
  [ADR 0004](../../../../protocols/workspace/doc/adr/0004-extensions-expand-their-input-selectors.md)
  (an input selector and the basis of its answer)

## Context

On Putnami CI, a run spends about 80 s between the end of its install and the
start of its session, for a narrow change as for a full one (run records of
the last seven days: 72 to 85 s on five pull request runs of about 216 tasks,
73 to 101 s on ten main runs of about 3,200 tasks). A CI runner is a fresh
clone: 18 of 419 runs reused a runner, so no record of an earlier run serves
the engine there.

Part of that time is the impacted selection. To decide which tasks read a
changed path, change impact asks the task index, for each changed path of a
project and each task of the workspace, whether one of the task's input
selectors reads the path. The matcher answered each question through
`Resolver.SelectsInput`, which resolves the query again: it checks the kept
answer by digesting its basis, a walk of the project and a pattern match of
each file it reaches. The same query was so digested once per changed path
and per task that names the selector.

Measured on 2026-11-27 at `4d6167d7` (main), on a 4-core Linux container, a
fresh clone after `putnami install`:
`putnami lint,test,build,validate --impacted --baseline 4f752128 --dry-run
--output=json`, the 27 files that commit changes, three runs:

| Measure | Before | After |
|---|---|---|
| Wall, median | 40.2 s | 16.5 s |
| Engine CPU under `TraceChangeImpact` (CPU profile) | 26 s | under 0.1 s |
| Planned tasks | 2,112 | 2,112, the same plan document byte for byte |

The rest of the selection is the runtime comparison of
[ADR 0103](0103-an-unchanged-extension-runtime-reaches-no-consumer.md), which
a hosted run reads from its install
([ADR 0104](0104-a-hosted-run-compares-runtimes-prepared-before-it.md)).

## Decision

The matcher `Resolver.Matcher` returns resolves each query once, and answers
every later path of that query from the expansion it resolved. A query is
named by the key its answer is kept at and the project directory its basis is
digested under. A resolution that fails is not kept: the next path asks
again, as the resolver does, and selects every path but a named-only input
meanwhile.

A matcher lives as long as one selection. A selection writes nothing in the
work tree: the expansion its first resolution checked is the one each later
resolution would check again. The keys of the run, computed after the
selection and while tasks write, read the resolver directly, which checks the
basis of each answer as before.

## Consequences

- An impacted selection checks the answer of each input selector query once,
  whatever the number of changed paths and tasks, beside the check of the
  answer when the selection primes it.
- Which tasks a run selects, their keys and their results do not change.
- A matcher kept past its selection, while files change, would answer from a
  stale expansion. The engine and `projects why` build one per selection.

## Alternatives considered

- **Memoize the basis digest in the resolver for the whole run.** Rejected:
  the run's tasks write while keys read the resolver, and a write would have
  to drop the digests it can move, the region rule of
  [ADR 0109](0109-a-write-drops-a-memoized-digest-only-when-it-can-move-it.md) applied to the
  basis. The selection alone needs no such rule, and it was 26 s of the 26 s.
- **Keep the basis digest across runs under a stat witness
  ([ADR 0113](0113-an-input-digest-whose-witness-holds-is-not-collected-again.md)).**
  Rejected for this cost: a CI runner is a fresh clone with no record, which
  is where the cost was measured.
