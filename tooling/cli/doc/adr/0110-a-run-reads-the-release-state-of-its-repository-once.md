# ADR 0110 — A run reads the release state of its repository once

- **Status**: accepted
- **Date**: 2026-11-26
- **Scope**: `@putnami/cli` (`internal/git`, `internal/jobs/cachekey`: the
  `releaseBaseline` runtime input)

## Context

A task that declares the `releaseBaseline` runtime input, such as
`validate~@putnami/go~api`, keys on its project's release baseline. The run
read it once per project directory and line pattern, and each read asked Git
in the project's directory: the repository's state (`rev-parse
--is-inside-work-tree --is-shallow-repository --show-prefix`), whether HEAD
names a commit, the line's last release (HEAD, every tag, then the tags HEAD
reaches) and, for an untagged line, whether the repository has any tag. All
but the prefix are facts of the repository, the same for every project.

Measured on 2026-11-26, on a 4-core Linux container, on an unchanged
`lint,test,build,validate --all` of this repository: 1,005 git calls, of which
316 `for-each-ref` and 495 `rev-parse`, about five per project with an
untagged line.

## Decision

1. **The run holds one reader of release baselines.** It reads the
   repository's state at the workspace root, whether HEAD names a commit,
   whether the repository has a tag, and each line's last release, once each.
   A project's prefix is the root's prefix followed by the project's path
   below the root.
2. **Only a directory the root's work tree holds is answered so.** The
   directory lies at or below the root, git names it by that path (it is
   reached through no link), and no `.git` lies at or above it below the root,
   which would start a nested repository or a submodule. Any other directory,
   or a root git does not answer for, is read in that directory, as before.
3. **What depends on the project is read for the project.** For a tagged
   line, the object the tag holds at the project and the breaking marker of
   the commits since that touch it are read in the project's directory, as
   before.
4. **The reads describe the repository when the run first asks.** A run holds
   one read of the tree facts (`protocols/job` ADR 0002); no task of a run
   moves HEAD or a tag that a later key of the same run must see.

## Rejected alternatives

- **Read every project's baseline from one listing of the tags, with the
  objects and histories of all projects at once.** The objects and the
  breaking marker are bounded by each project's path; batching them changes
  the Git queries, not only their number, for a state this repository does not
  reach on an unchanged run (its lines are untagged in a fresh clone).
- **Keep the reads between runs.** HEAD and the tags move with every commit
  and fetch; a run reads them in a few calls.

## Consequences

- A project whose line is untagged costs no git call of its own. On the run
  above, 316 `for-each-ref` and 495 `rev-parse` become 33 and 63.
- The baseline of every project is the value its own directory reads, which a
  test compares for the root, nested directories, a nested repository, a link
  and a directory outside the root.
