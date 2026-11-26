# ADR 0004 — A write of the run reads the cut again only for the keys it reaches

- **Status**: accepted
- **Scope**: `@putnami/cli` (`internal/jobs/cachekey`, the run's read of the
  candidate cut and of the tracked paths; `internal/jobs/scheduler`, which
  reads the tracked paths through it)
- **Amends**: [ADR 0003](0003-a-job-reads-the-candidate-cut-and-the-change-history-from-its-context.md)
  point 2; [ADR 0002](0002-a-job-reads-the-tree-facts-its-key-holds.md) point 4
  and `tooling/cli` [ADR 0068](../../../../tooling/cli/doc/adr/0068-an-emptied-optional-project-output-is-emptied-on-execution-and-on-an-empty-restore.md)
  point 6, for the read that gives the tracked paths (point 5); keeps ADR 0003
  points 1 and 3 and ADR 0002 point 2

## Context

ADR 0003 point 2 makes the cut follow the run: any write of the run marks the
read stale, and the next key or context that asks for the cut reads the whole
workspace's cut again, with one `ls-files`. A write under `.putnami` is first
asked of Git, once per run, whether Git excludes the directory.

Once `build-tsc` read the cut (`typescript/extension` ADR 0015), a run where
it is the only task that reads the cut paid for both. On the `web` case of
the extension counts test (`tooling/cli/internal/cli/e2e/orchestration`,
`build --all` on an application and its web project), the engine went from
20 Git calls to 22 while the jobs went from 1 to 0: one more `ls-files`,
after the writes of the project's own generate steps, and the `check-ignore`
of `.putnami`, after a store write. The decision
`exact-orchestration-counts-are-tests-of-this-repository` lets an engine
count take over a job read by at most the jobs' fall.

A key's cut is the listing at or below its directories (ADR 0003 point 1).
A write changes what the cut lists only at, below or above the written path
(a file created or removed there, a directory that holds candidates
removed), with two exceptions: a `.gitignore`, whose rules apply to the whole
directory that holds it, and a write under a `.git`, which holds the index
and the exclude rules of the repository of its directory.

## Decision

1. **The run keeps the writes since its read.** A write of the run records
   its paths, relative to the workspace root, as written and with its
   symbolic links followed, instead of marking the whole read stale.
2. **A key reads the cut again only when a write reaches it.** A key or a
   context reads the cut again when a recorded write lies at, below or above
   one of the directories it asks for, with case folded; otherwise it keeps
   the current read, whose listing there no write changed. The workspace's
   cut (`workspaceCandidateFiles`, a `git:**` digest of the workspace, an
   uncached job's context) asks for the root, which every write reaches. A
   read clears the recorded writes.
3. **A `.gitignore` and a `.git` reach their directory.** A written
   `.gitignore` counts as a write of the directory that holds it, and a write
   at or under a `.git` as a write of the directory that holds the `.git`: the
   workspace's own reaches every key.
4. **`.putnami` is asked of Git only when it matters.** A write under the
   workspace's `.putnami` directory reaches only a key whose directories hold
   it, in practice the workspace's cut. Git is asked whether it excludes
   `.putnami`, once per run, only when such a write alone would decide that a
   key reads the cut again, instead of at the write. Once the current read is
   known to exclude it, those writes are dropped.

5. **The tracked paths come from the cut the run read.** When the run has
   read the cut before it first needs the paths the index tracks (the
   `trackedFiles` fact of ADR 0002, or the check of `tooling/cli` ADR 0068 that
   an emptied output holds no tracked file), it takes them from that read,
   whose `ls-files --stage` lists every path of the index, with the index
   stamp taken before it, and does not read the index again. A later change
   of the index is seen through the stamp, as before.

The key still holds the read its context carries (ADR 0003 point 2): a key
records the read it took, and that read holds every write of the run at or
below its directories.

Measured on the extension counts test, cold runs:

- `web`, `build --all`: engine 20 and jobs 1 on main become engine 21 and
  jobs 0, 21 calls in all as before. Under ADR 0003 point 2 with ADR 0015 it
  read engine 22: the `check-ignore` is gone, and the `ls-files` that the
  writes of the project's generate steps cost stays.
- `typescript`, `lint,test,build --all`: engine 24 and jobs 1 become engine 23
  and jobs 0.
- `go`, `lint,test,build --all`: engine 17 becomes 16.
- `sdd` and the other cases are unchanged.

## Rejected alternatives

- **Keep ADR 0003 point 2.** A write in one project would keep costing a
  whole-workspace listing for a key of another project, and a run whose only
  cut reader is a project task would keep paying for `.putnami` writes it
  never reads.
- **Read the cut again only under the written directory.** One `ls-files`
  bounded to a prefix costs the same process as the whole listing, and
  splicing listings would need the cut's order and its one-entry paths, a
  submodule or a nested repository, to agree across reads.
- **Answer a project's `git:**/*` digest from the cut as well.** It would save
  the per-project listing of that digest, but it changes how a digest bounded
  by a prefix lists its files (ADR 0003 point 2, `tooling/cli` ADR 0041), and
  it is not needed to keep every count from rising.

## Consequences

- A key of a project no write of the run reached since the last read reads
  no cut again.
- A file written outside every declared output is still not seen until a
  write reaches its directory, as for the input digests.
- A write a symbolic link leads into another directory reaches that
  directory as well, by its resolved path.
