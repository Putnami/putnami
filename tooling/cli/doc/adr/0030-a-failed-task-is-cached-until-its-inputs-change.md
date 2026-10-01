# ADR 0030 — A failed task is cached until its inputs change

- **Status**: accepted
- **Scope**: `@putnami/cli` (`internal/store`, `internal/jobs`,
  `internal/output`), `@putnami/cli-model` (`JobResult`)

## Context

A re-run that changes nothing should not reproduce a failure it already
observed. Without a failure record the scheduler re-executes a whole gate
(about 900 tasks, about 20 minutes) to reach the same verdict.

The cache key already answers the question. It digests the task contract and
every declared input. If the key is unchanged, a deterministic task has nothing
new to say.

## Decision

A failed task records a **negative entry** in the LOCAL store, keyed by the
cache key that would serve its success. A later run with an unchanged key
replays that verdict (same `failed` status, same exit code, the original
diagnostics and error logs) instead of executing. The replay is consulted after
the local and remote positive lookups, so a positive entry always wins.

1. **One key.** Exactly what invalidates a positive entry invalidates a
   negative one. There is no second key and no extra input.
2. **Local only, by construction.** The record's address is derived from its
   own domain (`putnami/store/task-failure`). The remote-cache code only derives
   `TaskEntryAddress`, so it cannot name the record, and a failure never
   poisons another workspace.
3. **No declared outputs.** A failed task's output tree is untrusted. The entry
   is one small JSON record plus the ordinary `lastUsed` sidecar, so garbage
   collection treats it like any other blob.
4. **Fail closed.** A torn, foreign, malformed or non-failing record reads as a
   MISS. A wrong rejection costs one re-execution; a wrong acceptance serves a
   verdict that does not belong to these inputs.

### No clock, no TTL

A negative entry expires only on an input change, `--retry-failed`, a success
at the same key, or store GC. A task that fails once and passes next time with
the same inputs has a determinism bug (`prop/test-determinism`). A TTL would
hide it. A replayed failure the user believes spurious is evidence that the
task's inputs do not describe its outcome.

### What is not recorded

A recorded verdict must be a function of the cache key. `recordableFailure`
refuses every result that depends on something the key does not describe:

- **A timeout.** A deadline depends on host load. `JobResult.TimedOut` is the
  structural marker every deadline-to-failure site sets; no message is
  string-matched.
- **A hosted run.** It installs offline, so a failure from a missing dependency
  is not in the key of a task that does not declare the offline signal.
- **A sensitive leak** (`FailureSensitiveLeakDetected`). It rests on per-run
  artifact paths, byte samples and provisioned process capabilities.
- **A drift verdict** (`generated-output-drift`). It compares output with the
  worktree ([ADR 0034](0034-a-declared-output-polices-its-own-drift.md)).
- **A canceled or skipped result.** Only status `failed` is recorded; a skip
  has its own caching rule (`isCacheableResult`).
- **A wrapper bail-out.** A job that failed before its own logic ran (an
  unavailable Go toolchain, an empty project) failed for a property of this
  host. The `resultEmittedMeta` guard the cached-skip path applies is reused.

A plan-level shared node records one observation per key per run, so the
attempt count means runs, not plan nodes.

### A bypass reads nothing and records nothing, but a success still forgets

A bypassed run (`--no-cache`, or `--no-cache-projects` over the task's
closure) never writes a verdict at the key it refused to read. A SUCCESS is not
a published verdict: it proves the recorded failure is stale, and deleting
stale state costs at worst one re-execution. So a success deletes the record
however the run was configured, and a bypassed failure leaves the previous
record untouched.

To make that possible, the scheduler computes the key of a bypassed cacheable
task and uses it for exactly two things: the identity its bypassed dependents
fold into their keys, and deleting a disproved record. The engine hands the
scheduler the local store under `--no-cache`; the per-job bypass refuses every
lookup, restore, lease, publication and replay. The remote cache and the
opportunistic store GC stay off. A task that cannot use the cache has no key
and nothing to forget.

### The key does not depend on the run's selection

`--projects <p>` and `--impacted` give the same task the same key for the same
inputs, so a green run under either clears what the other recorded. The
planner's fixpoint walks the workspace dependency graph, not the selection, to
add every upstream job a `^` reference needs. The selection decides which tasks
run, never how one is keyed.
`TestCacheKeyIsTheSameUnderEverySelectionThatPlansTheTask` and
`TestCacheKeyIgnoresTheRunSelection` pin this.

### Not gated on `cache.deterministic`

Few tasks declare `cache.deterministic`; gating on it would make the feature
inert on the gate it exists to speed up. Points 2 and 3 plus the escapes are
the safety net.

### A replay is a failure, not a cache hit

`MarkReuse` is not called: it would set `CacheHit`, turn `Outcome()` into
`"cached"` and count the task as a hit. A replayed failure has the same
status, exit code and effect on dependents as an executed one, so dependents
need no scheduling code. Its provenance travels in `JobResult.ReplayedFailure`,
which only renderers read. The renderer prints the original failure output
verbatim plus one line naming the age, the attempt count and `--retry-failed`.

## Consequences

- A re-run that changes nothing costs seconds and still exits non-zero with
  the same diagnostics.
- A flaky task whose inputs do not explain its outcome becomes loud.
  `--retry-failed` is the escape while the determinism bug is fixed.
- The store grows by one small record per distinct failing key, metered and
  evicted by the existing byte budget and idle reclaim.
