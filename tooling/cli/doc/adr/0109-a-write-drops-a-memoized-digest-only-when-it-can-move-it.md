# ADR 0109 — A write drops a memoized input digest only when it can move it

- **Status**: accepted
- **Date**: 2026-11-26
- **Amends**: [ADR 0106](0106-a-write-under-an-excluded-putnami-keeps-the-candidate-digests.md)
  (a digest that lists its Git candidates from the run's cut no longer asks the
  exclusion, and the exclusion covers the plan's outputs),
  [ADR 0041](0041-git-candidate-file-inputs.md) (a `git:` digest lives in the
  run's memo until a write can move it), and point 4 of the job protocol's
  [ADR 0004](../../../../protocols/job/doc/adr/0004-a-write-reads-the-cut-again-only-for-the-keys-it-reaches.md)
- **Scope**: `@putnami/cli` (`internal/store`, `internal/jobs/cachekey`)

## Context

A run memoizes each input digest with the region its files can lie in, and a
task's write, its outputs or a restore, drops every digest whose region it
reaches. A workspace-wide pattern such as `**/putnami.json` or `git:**` has the
whole workspace as its region, so every write of the run drops it, and the
next task that keys on it walks the workspace again.

Measured on 2026-11-26 at `9682e140`, on a 4-core Linux container,
`lint,test,build,validate --all` run three times on the tree of a complete run:
a median of 35.6 s wall, a maximum ready wait of 19.6 s, an engine CPU of
65.4 s of the run's 76.8 s. A CPU profile of the engine put half of it in
computing cache keys, and the task records summed 47.3 s of key time, 32.2 s
of it in the 123 `validate~@putnami/sdd~specs` tasks, which key on the
workspace's project configs, features and specs, and 16.8 s in `lint~docs` and
`validate~@putnami/sdd~features`, which key on the workspace's `git:**`. The
run wrote about 1,000 times, nearly all restores: under `.putnami/out`, under
each project's `.gen`, and optional outputs that do not exist.

## Decision

1. **A digest keeps what it read.** A memoized digest records the directory
   it was collected in, its patterns and the files it selected. It has such a
   witness only when the names of the files alone decide its selection: no
   input selector, no empty pattern set, no `.`, `..` or empty segment and no
   character class in a pattern, no glob character in the directory, the
   directory and each ordinary pattern's static prefix reached through no link,
   no glob before the last segment of a pattern that is not recursive (Go's
   glob follows links there), `git:` candidates listed from the run's cut, and
   no selected file read through a link, the file itself or a directory on its
   way. That last check is made at the first write that asks. A candidate the
   cut lists counts as read even where it is absent, as an unstaged deletion
   is. Any other digest keeps the region rule, and one whose ordinary
   patterns match a symbolic link, selected or not, or hold a glob before the
   last segment of a pattern that is not recursive, is not memoized at all: a
   write can create or change a link's target anywhere, outside its region
   too.
2. **A write moves a witnessed digest only when it can.** A write at or under
   the witness's directory, through no link, moves the digest when a file it
   read lies at or under a written path (a change or a removal), or when a
   file the write left there, read once per write, could be selected by its
   patterns, as the collector selects: an exclusion removes it, and a recursive
   pattern does not reach below the directories its walk skips (`node_modules`,
   `.git`, `.putnami`, `out`, `dist`, `vendor`). A link the write left moves it,
   and so do a `.gitignore`, left or named, and a path under `.git` when a
   `git:` pattern reads candidates, as in `protocols/job` ADR 0004. A write above the directory,
   one through a directory whose file system folds case (its name with the
   case of its letters swapped finds it), or one tree of more than 20,000
   entries keeps the region rule.
3. **A candidate the cut decides waits for the cut.** When only a `git:`
   pattern could select a file the write left, the digest moves only if that
   file is a candidate. When the digest listed its candidates from a read of
   the run's cut, it stays, pending: the next lookup reads the cut, which the
   write already made stale, and keeps the digest when that read lists the
   same paths, modes and unmerged marks as the read it listed from; otherwise
   it computes it again. A digest whose candidates Git listed itself asks the
   exclusion of ADR 0106, as before.
4. **A write under an ignored output leaves the cut as it is.** The rule of
   ADR 0106 for `.putnami` covers every output directory the plan declares: a
   write at or under one, through no link out of it, changes nothing the cut
   lists when Git's rules exclude that directory as a whole and the current
   read lists nothing at or below it. The one `check-ignore` the run asks
   about `.putnami` names every such directory Git can answer for (none behind
   a link or in a nested repository), and it is asked only where it was asked
   before: when writes under `.putnami` alone would decide that a key reads the
   cut again. Until then, and for the rest of the run once a write names a
   `.gitignore` or a path at, under or above `.git`, or a directory above an output where
   a `.gitignore` between it and the output differs from what was read when
   Git was asked, a write under another output reads the cut again, as
   before.
5. **No key moves.** A kept digest is the digest the collection after the
   write gives: the write added, removed and changed none of its files.

## Rejected alternatives

- **Exclude every directory the walk skips from every region.** An ordinary
  pattern that names `.putnami` or `out` before its `**` still selects below
  it; only the per-file rule knows which pattern skips what.
- **Ask Git about an output directory at its first write.** A `.gen` of each
  project would cost a `check-ignore` per project and per run, and the
  orchestration counts of a fixture would rise by one where no `.putnami`
  question was asked.
- **Drop a digest a write can select by name, whatever the cut.** Every
  restore under a `.gen` would keep dropping the workspace's `git:**`, which
  any name matches: 16.8 s of the run's key time.
- **Keep the digests across runs.** It would remove the first computation of
  each digest too, but needs a witness of every directory listing on disk
  between runs. The run's own writes were the measured cost.

## Consequences

Measured as above after the change, together with
[ADR 0110](0110-a-run-reads-the-release-state-of-its-repository-once.md) and
the cached skip of a library's `build~infra`: 25.1 s wall (median of three), a
maximum ready wait of 9.4 to 9.8 s, an engine CPU of 38.8 to 40.2 s, and 17.1
to 18.0 s of key time summed over the tasks. The orchestration counts of this repository's
fixtures do not move.

- A write whose paths no longer exist, such as an optional output a task did
  not write, moves no witnessed digest.
- A file written outside every declared output is still not seen until a
  write reaches it, as before.
- On a case-insensitive file system the comparison of written and read paths
  folds case, as the regions do: a write to another spelling of a read file
  moves the digest.
- A write through a directory whose file system folds case keeps the region
  rule, so on such a volume the witness decides nothing.
- As for `.putnami` under ADR 0106, the exclude file Git's configuration names
  (`core.excludesFile`) is read when Git is asked and not again: a task of the
  run that rewrites it, inside the workspace, is outside the rule.
