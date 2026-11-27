# ADR 0127 — A fresh clone digests its files: the Git index is no witness

- **Status**: accepted
- **Date**: 2026-11-27
- **Scope**: `@putnami/cli` (`internal/store`, `internal/fileindex`: the input
  digests of cache keys on a checkout with no record)
- **Related**: [ADR 0112](0112-the-engine-reads-the-git-index-and-the-work-tree-itself.md)
  (the engine reads the Git index), [ADR 0113](0113-an-input-digest-whose-witness-holds-is-not-collected-again.md)
  (an input digest whose stat witness holds is not collected again),
  `protocols/workspace`
  [ADR 0009](../../../../protocols/workspace/doc/adr/0009-a-files-stat-decides-that-it-is-unchanged.md),
  which already rejected Git's own index as the stat-to-digest map

## Context

The input record of ADR 0113 and the file index of ADR 0009 answer a digest
from a stat witness an earlier run on the same machine recorded. A Putnami CI
runner is a fresh clone. Of the 419 runs of this repository on Putnami CI over
the seven days to 2026-11-27 that recorded a gate time (`ci_gate_usec` in
the run records `putnami cloud ci list --age 168h --limit 0 --output=json`
returned that day), 18 carried `ci_runner_reuse`, about one in 23. That is an upper bound on the runs where these witnesses can hold, not
a count of runs where they held.

The Git index of a fresh clone names, for each tracked file, the object ID of
the content it checked out. Turborepo keys a tracked file by that object ID.
ADR 0009 rejected the index as the machine-local map from stat to digest; the
question here is whether it could stand for a digest where no record exists.

An index entry describes the work-tree bytes only when all of these hold:

- The entry is stat-clean and not racily clean: its stat equals the file's,
  and the file is older than the index at the precision the file system and
  the index keep. The index stores stat fields truncated to 32 bits, and
  `core.checkStat=minimal` or `core.trustctime=false` weaken Git's own check.
- No conversion applies between the blob and the work tree: no `filter`,
  `ident`, `working-tree-encoding` or end-of-line conversion on checkout. This
  repository has none of the first three, and converts one file:
  `java/samples/gradle/gradlew.bat` is checked out with CRLF from an LF blob
  (`*.bat text eol=crlf`), so its object ID is not the hash of its work-tree
  bytes.
- The entry is not skip-worktree, assume-unchanged, intent-to-add, unmerged,
  a submodule, or a sparse-index directory entry, and `core.symlinks` is not
  false (which checks a link out as a plain file).
- The file is tracked: an untracked or ignored input, generated sources and
  installed dependencies included, has no object ID.

Even where they hold, the object ID is not what the keys read. A key folds,
for an ordinary file, the SHA-256 of its raw bytes; for a Git candidate, its
mode and bytes under a domain prefix; for a symbolic link, its target; for a
project configuration or a version stamp, a scoped reading of its bytes. An
object ID is the SHA-1 (or SHA-256) of the blob with its header, and covers
no mode. Standing one for the other needs either keys over object IDs, which
moves every key, or a table from object ID to digest, which a fresh clone does
not have unless it comes from another machine, and then that machine decides
this one's keys.

Measured on 2026-11-27 on a 4-core Linux container, on fresh clones of this
repository at `45647bc5` (13,239 tracked files, 92 MiB), on an idle
container, with
`git ls-files -z | xargs -0 -P4 -n500 sha256sum > /dev/null`:

| Measure | Value |
|---|---|
| SHA-256 of every tracked file, right after the clone, four clones | 0.06 to 0.07 s |
| The same with the page cache dropped (`echo 3 > /proc/sys/vm/drop_caches`), four clones | 0.36 to 0.43 s |
| `git ls-files -s` | 0.01 s |

On Putnami CI, the engine spends 72 to 101 s between the start of the gate
and the start of its session (the run records of five pull request runs and
ten main runs of 2026-11-27, read with `putnami cloud ci view` and the
`session:end` record of `putnami cloud ci logs`).

## Decision

A run on a checkout with no record digests the files its keys read, through
the file index, as it does today. The Git index is not a witness of a file's
digest.

## Consequences

- The keys stay what they fold today; no key moves.
- A fresh clone pays under about 1 s to hash every tracked file, even with a
  cold page cache and a loaded machine: about one percent of the engine's
  time before the first task on CI. The cost of a fresh clone lies in the walks, the probe and the
  implicit install, which other decisions address.

## Alternatives considered

- **Key tracked files by their object ID.** Rejected: every key would move,
  the ID covers no mode and no converted file, and the saving is bounded by
  the second above.
- **A shared table from object ID to SHA-256.** Rejected: a fresh clone would
  read another machine's answer for its keys, for the same bound.
- **An object ID as a witness of a recorded digest when its stat witness
  fails**, on a machine that keeps its records (a checkout whose files Git
  rewrote with the same bytes). Not taken: CI has no recorded digest to
  witness, and on a machine that keeps its records the files Git did not
  rewrite already hold ADR 0113's stat witness, while hashing the rewritten
  ones again stays under the same bound.
