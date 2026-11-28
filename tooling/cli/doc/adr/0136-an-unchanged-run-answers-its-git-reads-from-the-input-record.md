# ADR 0136 — An unchanged run answers its Git reads from the input record

- **Status**: accepted
- **Date**: 2026-11-27
- **Extends**: [ADR 0113](0113-an-input-digest-whose-witness-holds-is-not-collected-again.md)
  (the input record and its rule) to the Git reads of a run, and
  [ADR 0112](0112-the-engine-reads-the-git-index-and-the-work-tree-itself.md)
  §7 (what stays with Git) by keeping Git's answer instead of asking again
- **Scope**: `@putnami/cli` (`internal/git`: the held reads and their
  witnesses; `internal/engine`: the record opens before the version snapshot;
  `internal/cli`: an interactive command's versions), the SDK (`gitread`: the
  hook that answers a call; `linetag`: its calls go through it)

## Context

ADR 0112 read the index and the work tree in-process and left Git the reads
of objects, history, `status` and the configuration. Each run asks them
again, although nothing they read moved. On an unchanged run of this
repository, the engine still started 17 Git processes on a shallow clone and
39 on a full clone with tags; a child `putnami cloud token --for cache`, which
computes the run's versions the same way, started 45 more. The scale bench
counts 15 on its workspace, where Nx starts none. Every workspace pays these
calls: their number grows with the version lines, not with the tasks.

## Decision

1. **A run answers a held Git read from the input record.** The SDK's `gitread`
   takes an optional source of answers (`gitread.UseHeld`); every call it runs
   in the process's environment, and every call of `linetag`, asks it first,
   with the environment Git runs with. The CLI installs one for the run's
   input record (ADR 0113) once the record is open: the run opens its file
   index and record before it reads the version snapshot, and an interactive
   extension command opens the record around the reads of its versions. An
   answer is keyed by the directory, the arguments, the standard input, the
   git executable's path, the user, and every `GIT_*`, `HOME`,
   `XDG_CONFIG_HOME`, `LANG` and `LC_*` variable of that environment; a call
   under a variable that points Git at another repository holds nothing (ADR
   0112 §5).

   ```mermaid
   flowchart LR
     call[a git call] --> table{in the held table?}
     table -- no --> git[run git]
     table -- yes --> held{record answer whose parts all keep their stats?}
     held -- yes --> answer[Git's earlier answer, no process]
     held -- no --> witness[take the witness] --> git2[run git] --> again{witness stats unchanged?}
     again -- yes --> keep[keep the answer with the witness]
     again -- no --> drop[keep nothing]
   ```

2. **Each held call is a function of a witness of stats.** The table names the
   calls by their arguments, in three classes:
   - **The repository**: HEAD and the branch (`symbolic-ref`, `rev-parse`),
     the abbreviated name, the clone's depth, the common directory, the work
     tree's root, the commit time, the tags at HEAD, the tag names and the
     objects they peel to, and the history calls that name a commit by its full
     name (the tags HEAD reaches, the commits from one to HEAD under
     pathspecs, the files a commit changes). Their witness is HEAD, the
     `commondir` and `config.worktree` files and a linked worktree's `.git`
     file, the absence of a `.git` between the directory and the work tree's
     root, every file the configuration reading depends on, `packed-refs`,
     `shallow`, the grafts, the alternates, the reftable, every directory and
     file under `refs`, the object directories (`objects`, `objects/pack`,
     `objects/info` and each of the 256 loose directories: a new object can
     lengthen an abbreviated name), each link on the way to the directory, and
     the git executable. A `.git` directory is no part: every lock Git takes
     moves it. A repository that borrows objects (`objects/info/alternates`)
     holds nothing.
   - **The work tree**: `status` (both forms), `diff HEAD`, the untracked and
     the ignored listings. Their witness adds the index, every tracked path,
     every untracked path, the directories and rule files the untracked walk
     reads (the cut's witness, ADR 0113 §3) and the attribute files a diff
     reads (`info/attributes`, `/etc/gitattributes`, `core.attributesFile` or
     its default). A directory's answers of this class share one entry and one
     witness. A repository with a gitlink, an untracked nested repository or
     more than 200,000 tracked paths, whose witness would crowd the record's
     bound of parts, holds no answer of this class.
   - **The ignore rules**: `check-ignore` of named paths. Its witness is the
     index, each path, the `.gitignore` of each directory from the root down
     to it, `info/exclude` and the global ignore file.

   The configuration reading itself (`git config --list --show-origin`) is kept
   in the same table under the files ADR 0112 §4 stamps, when it is a reading
   the process keeps. A file Git reads outside the repository (a
   configuration, ignore or attribute file, the executable) is witnessed with
   each link on the way to it and the file it resolves to.

3. **An answer is checked with stats taken at the call.** The record's stats
   of a run (`Record.Hold`) are taken once and dropped only by the writes the
   run names; repository code, a hook or a task can change the repository
   between two calls of one run. A held answer states its parts again at each
   call, and the run drops the stats it took before its hooks once they ran.

4. **An answer is kept only when its witness did not move while Git read
   it.** The witness is taken before the call, and its paths are stated again
   after it; a stat that moved, Git's own refresh of the index included,
   keeps nothing. The record's rule applies (ADR 0113 §2): a part written in
   the two seconds before the run opened its record is not recordable, and a
   run that only plans keeps nothing. A process keeps the last witness it took
   by directory and class, and the untracked walk's paths in the record, so
   that a run whose witness holds reads no cut for it, and one after an edit of
   a tracked file reads none either. A witness takes the configuration
   reading at most once per process: when that reading is not one the process
   keeps, the call is not held and Git answers it.

5. **What the witness cannot see stays with Git.** A call outside the table,
   a call with an environment of its own, any state the in-process reader
   steps aside for, and a non-zero exit run Git, as before. The common
   directory is read in-process (the reader's discovery) and falls back to
   Git the same way.

## Rejected alternatives

- **Read the objects in-process** (commit times, peeled tags, history): it
  needs loose and packed objects with deltas, a second reading of Git's object
  store for answers that a stat witness already proves unchanged.
- **Key the answers by HEAD alone**: a new tag, a staged change or a new
  untracked file moves an answer without moving HEAD.
- **Hold the whole version snapshot as one value**: the snapshot, the version
  lines, the tree capture and the store root ask different questions at
  different moments of the run; one table of Git calls holds each where it is
  asked, and a call the table does not name keeps asking Git.

## Consequences

- An unchanged run starts no Git process for these reads, on a shallow or a
  full clone (`TestAnUnchangedRunStartsNoGitProcess`); a run after a change
  asks Git again for each answer whose witness moved
  (`TestAnUnchangedRepositoryAnswersItsGitReadsFromTheRecord`). The pull
  request that adopted this record holds the measurements.
- A run that records reads the cut once more for the work tree's witness,
  and a run that opens no repository otherwise reads Git's configuration for
  the witness; the orchestration ceilings say so.
- Keys and results do not move: an answer is Git's own output, and the record
  answers it only while every stat Git's reading depended on holds.
- A machine-level process can hold the same answers in memory and drop one
  on a file event; the record stays the fallback.
