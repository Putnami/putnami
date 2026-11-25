# ADR 0106 — A write under an excluded .putnami keeps the digests of Git candidates

- **Status**: accepted
- **Date**: 2026-11-25
- **Amends**: [ADR 0041](0041-git-candidate-file-inputs.md) (a `git:` digest
  lives in the run's memo until a write can move it), and extends point 2 of
  the job protocol's
  [ADR 0003](../../../../protocols/job/doc/adr/0003-a-job-reads-the-candidate-cut-and-the-change-history-from-its-context.md)
  from the run's cut to the memoized input digests
- **Scope**: `@putnami/cli` (`internal/store`, `internal/jobs/cachekey`,
  `internal/jobs/scheduler`)

## Context

A run memoizes each input digest with the region its files can lie in, and a
task's write drops every digest whose region it reaches. A `git:` pattern
without a static prefix, such as a project's `git:**`, matches candidate paths
with `..` segments, so its region is unbounded: every write of the run drops it.
Almost every write lies under the workspace's `.putnami/out`, the command output
directories. The next task that keys on the digest lists the candidates again
with Git: one `rev-parse` and two `ls-files`.

Which task pays depends on the dispatch order. In a fixture of 3 projects whose
`lint`, `test` and `build` each key on their project's `git:**` and write one
report under `.putnami/out`, a run of the 9 tasks on one worker made 42 git
calls, 21 `ls-files` and 15 `rev-parse`: each task listed its project's
candidates again after the task before it wrote (measured on 2026-11-25 on a
4-core Linux container, `TestAWriteUnderAnIgnoredPutnamiListsNoCandidateAgain`).

`.putnami` is not ignored in every workspace. Where Git does not exclude it, a
file written there is a candidate, and a digest that keys on it must move.

## Decision

1. **A write where Git lists no candidate moves no digest of candidates.** A
   memoized digest records the part of its region where it reads files that
   are not Git candidates: the region of its includes other than `git:`
   patterns, empty when every include is one. A write that reaches the region
   only outside that part, and lies where Git lists no candidate, keeps the
   digest: it adds no candidate, removes none and changes the bytes of none.
   Every other write drops the digests it reaches, as before.
2. **Where Git lists no candidate is decided as for the cut.** A written path
   lies there when it is at or under the workspace's `.putnami` directory, Git's
   rules exclude that directory as a whole, and the run's read of the cut lists
   nothing below it: the rule of ADR 0003 point 2, with the same once-per-run
   `check-ignore`. A candidate lies below it when its path names it, compared
   with case folded, or when a directory on its path is a symbolic link that
   leads there: its digest reads the bytes a write there changes. Each
   directory of the cut is stated once per read. Where Git does not exclude `.putnami`, or tracks a file below
   it, no write there is spared, so the rule holds whether or not `.putnami` is
   ignored. A path that a symbolic link on it leads out of `.putnami` is not
   there, `.putnami` itself being such a link: the write lands where the link
   leads. The cut and the digests decide alike. A read of the cut a write of
   the run made stale is read again before the answer, so a run that stops
   ignoring `.putnami` sees the files then below it as candidates; whether Git
   excludes `.putnami` stays the answer of the run's one `check-ignore`.
3. **The question is asked only when a digest depends on it.** A write that
   reaches no digest of candidates asks nothing. The first one that does asks
   Git about `.putnami` and reads the cut when the run has not read it yet or
   a write made the read stale. A cut that could not be read excludes nothing.

## Rejected alternatives

- **Bound every region below `.putnami`.** An ordinary pattern can select a file
  under `.putnami`, and a `git:` pattern does when Git does not exclude it; a
  region that left it out would keep a digest a write moved.
- **Decide with `check-ignore` alone.** A file the index tracks below an ignored
  directory is still a candidate, and a write to it changes its bytes.
- **Ask eagerly at the first write under `.putnami`.** A run whose keys read no
  candidate would pay a `check-ignore` and a cut read for nothing.

## Consequences

- In the fixture above, the run makes 26 git calls: each project's candidates
  are listed once, by its first task, plus one `check-ignore` and one read of
  the cut. The other orchestration counts of this repository do not move: their
  candidate digests are listed from the cut, or keyed once per run.
- A digest that mixes `git:` patterns with ordinary ones keeps its entry only
  when the ordinary patterns' region does not reach the write, such as
  `git:**/*` with `src/**`. One whose ordinary patterns reach `.putnami`, such as
  `**/*.json` at the workspace root, is still dropped.
- No key moves: a spared digest is the digest the listing after the write gives.
- A write is known by the paths a task declares. A link inside a declared
  output that leads out of `.putnami` into sources is not followed: a file a
  task writes through it is written outside every declared output, which no
  digest sees until the next write that reaches it, as for any such file.
