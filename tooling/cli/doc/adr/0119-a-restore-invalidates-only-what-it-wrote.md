# ADR 0119 — A restore invalidates only what it wrote

- **Status**: accepted
- **Date**: 2026-11-27
- **Extends**: [ADR 0108](0108-a-cache-hit-reads-its-entry-and-writes-only-what-differs.md)
  (a restore writes nothing to a destination that already holds the output)
  to what the restore invalidates afterwards, and
  [ADR 0109](0109-a-write-drops-a-memoized-digest-only-when-it-can-move-it.md)
  (an invalidation judges only the digests whose region its write overlaps)
- **Scope**: `@putnami/cli` (`internal/jobs/scheduler`, `internal/store`)

## Context

Since ADR 0108, a cache hit whose outputs are already on disk writes nothing.
The rest of the restore still acted as if it had written every recorded
output:

- it dropped every memoized input digest the outputs' paths could move, and
  recorded the write against the run's read of the candidate cut;
- it cleared the memoized source-v1 bindings of the project, or of the whole
  workspace for a workspace-rooted output, before and after the
  materialization, so the next stamp or key read the Git cut again;
- it rewrote the build stamp of a restore whose entry records `.gen`, which
  reads those bindings.

An invalidation also judged every memoized digest of the run, one region
comparison each, whatever the write reached.

Measured on 2026-11-27 on a 4-core Linux container, signed out, at
`299343dc`, the medians of three unchanged runs of
`lint,test,build,validate --all --enforce-coverage` after a priming run, 2,194
hits: a CPU profile of the engine puts 5.1 s in `restoreDeclaredCacheHit`, of
which 1.7 s in `invalidateWritten`, 1.0 s materializing, 0.6 s re-stamping and
1.6 s replaying the session records (out of this decision's scope).

## Decision

1. **The store says whether a restore counts as a write.** It does when the
   store wrote the destination. A destination the store left as it stood,
   because it held the output, counts as written only when a directory or
   file the comparison stated there changed (its change time) at or after
   the start of the run's reads of the tree, the start of the process less
   2 s, or when the platform gives no change time. Another session of the same
   worktree may have written those bytes after a read this run memoized, and
   the restore then drops that read as it would drop its own write.
2. **A completed restore invalidates the outputs that count as written, and
   only them.** An output also counts as written when the restore emptied it
   (below), or when the restore detached the command's output directory
   from a link or created it, which counts every output under that
   directory. The memoized digests, the run's candidate cut and the memoized
   source bindings are invalidated for those outputs alone, and for the build
   stamp when the restore's re-stamp rewrote it; a restore where none counts
   invalidates nothing. A destination left as it stood, unchanged since the
   run began to read the tree, holds the bytes every read of this run saw, so
   no digest, cut or binding read of it moved. An emptied output whose
   destination is absent before and after the emptying is judged the same
   way: it counts as written unless the closest directory above it that
   exists is no link and has not changed since the start of the run's reads
   of the tree, where the absence predates every read of the run, which the
   restore leaves as it stands. Counting every
   emptied output as written made an unchanged run of this repository read
   the Git cut 18 more times (amended 2026-11-27).
3. **A failed restore invalidates every recorded output**, as before: it may
   have written any of them.
4. **The bindings are invalidated once, after the materialization.** A binding
   is computed and memoized under the scheduler's version-stamp lock, so one
   computed before or during the write is memoized before the invalidation
   takes the lock, and the invalidation that follows the write clears it. The
   invalidation before the write cleared nothing that one does not.
5. **The memo indexes its digests by region.** Each memoized digest is found
   by the roots of its region: an invalidation looks up the roots at and above
   each written path by name, the roots below it by a range of the sorted
   roots, and the digests whose region is unbounded. It judges those with the
   rule of ADR 0109 and no other; a digest outside them has a region the write
   does not overlap, which that rule keeps. A write that names no bound (a
   source rewriter) still judges every digest.

## Rejected alternatives

- **Skipping the materialization when the outputs match.** It is what ADR
  0108 already does: the comparison it runs is the cost of knowing that they
  match, about 0.1 s of the 1.0 s above. The rest, 0.9 s, publishes the
  restores that write: on this repository, the `.gen` tree that the `build`
  and `test` generate tasks of a TypeScript project share. Their entries
  differ in `.gen/generate-result.json`, which records the command's mode, so
  each restore undoes the other's, and a tree that differs is swapped whole:
  153 such files on an unchanged run, and the whole `.gen` of
  `sites/putnami.dev`, 1,254 files and 42.5 MB. The `.gen/version.json` that
  a run also rewrites, 161 of them, come from the tasks that execute on every
  run, which stamp a fresh build time, not from restores.
- **Re-stamping only when a written output replaced the stamp.** A held
  `.gen` can hold the stamp of another session's identity, or a binding this
  session has since moved, so the restore still re-stamps every output that
  contains the stamp, and invalidates the stamp when that rewrote it. The stamp reads the memoized bindings, which decision 2
  no longer clears for a held restore, so it costs little.
- **One read of the Git cut for the run's stamps.** The stamps read the
  memoized bindings; the repeated reads came from the restores that cleared
  them without writing. Decision 2 removes those, and a run-wide read would
  serve a stale binding after a restore that does write.

## Consequences

Measured the same way, both sides primed on the same machine, medians of
three runs:

| | `299343dc` | This decision |
|---|---|---|
| Wall | 31.8 s | 28.2 s |
| Engine CPU | 45.7 s | 36.7 s |
| Longest ready wait | 12.0 s | 8.5 s |
| `restoreDeclaredCacheHit` | 5.1 s | 3.4 s |
| of which `invalidateWritten` | 1.7 s | 0.6 s |
| of which the stamp | 0.6 s | 0.3 s |
| of which the session records | 1.6 s | 1.7 s |

The CPU profile of the restore was taken one commit before the last repair,
which re-stats the files a held destination is compared on and invalidates a
stamp a held restore rewrites; the three rows above it were measured after it.
The engine CPU falls by more than the restore's own: a digest a restore no
longer drops is not computed again by the next key that reads it. Keys,
results and session records are unchanged.

What a hit still pays is the replay of its session records, and the restores
of the shared `.gen` trees, which write.
