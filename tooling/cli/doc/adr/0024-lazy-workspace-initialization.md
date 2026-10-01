# ADR 0024 — Lazy workspace initialization

- **Status**: accepted
- **Scope**: `@putnami/cli`, workspace probes, artifact restoration,
  installation output

## Context

A new worktree has committed declarations and extension pins but no
`.putnami/workspace-index.json` or installation marker. An agent host can
start MCP before the first job command. Restoring extension descriptors alone
does not yield provider-derived project identities and dependencies, and a full
installation would also run dependency installers, Cloud setup, and context
generation.

## Decision

### 1. Graph readiness is separate from full installation

MCP prepares its disposable workspace index lazily, on the first tool request
that needs workspace facts, through the provider probes job planning uses. The
preparation has a 60-second deadline, separate from the five-second extension
preparation of [ADR 0022](0022-analysis-prepares-only-locked-read-surfaces.md).
It keeps exact extension selection and never runs `workspace-install`,
`workspace-sync`, Cloud setup, or context generation. Core never synthesizes
language facts.

A missing index after failed preparation stays an explicit error, and a later
request can retry. An unresolved locked extension is not an absent provider:
failed preparation never publishes an apparently complete core-only index. MCP
job previews use the engine preview path, not this preparation.

### 2. Index publication is coordinated

Reconciliation coordinates concurrent callers for one worktree through
`workspace-index.lock`, rechecks freshness after acquiring it, and publishes a
validated snapshot atomically. A long-lived process checks whether the recorded
index changed before reusing a cached workspace. The index and the resolved
graph describe the same provider results even when another process created the
index.

Waiting for the lock is bounded and reports contention after one second.
Cancellation or a timeout never bypasses an active owner's lock. When
permissions prevent opening the lock, synchronization warns and may resolve the
provider view in memory, but that unlocked run publishes no index.

### 3. Restoration runs concurrently, merges deterministically

Independent artifact restorations and stale provider probes run with bounded
concurrency. Results are collected by stable identity; diagnostics and shared
metadata are merged in deterministic order. Hook sequencing is preserved, and
operations that share writable state are serialized. Only a successful full
installation records the installation marker.

### 4. Installation output is compact

Installation shows a compact progress view by default: in place on a terminal,
a concise final report when redirected. A check that changed nothing leaves no
permanent line. Changed-artifact reporting comes from typed results, never from
parsing logs; when an installer reports only that it ran, the summary names the
action without claiming a filesystem change. Warnings, failures, and verbose
mode keep full detail. Human progress never enters machine stdout or the MCP
JSON-RPC stream. Terminal control sequences require a terminal at the actual
progress writer. A completed index publication stays in the summary when a
later installer fails, without claiming the installation succeeded.

## Rejected alternatives

- **Full installation at every MCP start.** It brings dependency and service
  configuration effects into analysis.
- **An empty graph when the index is missing.** It hides projects and
  dependencies.
- **More scheduler parallelism alone.** Sequential preparation and verbose
  output would remain.
- **Filtering logs for words like "installed".** The report would depend on
  extension wording.

## Consequences

- A fresh MCP session gets the project graph without a full installation.
- Cold graph preparation may compile a local provider runtime within its
  budget, or report an unavailable graph.
- Worktrees keep separate mutable state while sharing verified artifact bytes.
- The installer job protocol does not prove which dependency files changed;
  finer attribution needs provider-owned results.
