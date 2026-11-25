# ADR 0002 — A job reads from its context the tree facts its cache key holds

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/job` (`tree`),
  `go.putnami.dev/protocol/extension` (runtime input names),
  `@putnami/cli` (`internal/jobs`), `go.putnami.dev/sdk/extension`
  (`treefacts`)
- **Applies**: `tooling/cli` [ADR 0071](../../../../tooling/cli/doc/adr/0071-an-extension-command-calls-the-core-over-a-host-rpc-channel.md)
  point 1 to the job context; keeps
  [ADR 0059](../../../../tooling/cli/doc/adr/0059-a-cache-key-describes-the-tree-not-the-commit.md),
  [ADR 0060](../../../../tooling/cli/doc/adr/0060-the-base-version-is-not-a-cache-key-input.md) and
  [ADR 0062](../../../../tooling/cli/doc/adr/0062-a-release-baseline-input-names-the-baseline-not-the-commit.md)

## Context

Jobs ask Git what the engine reads or can read once per run. A no-cache gate
of this repository on 2026-11-24 counted 1,120 Git calls made by jobs: `ls-files` 452, `rev-parse` 286, `cat-file` 163,
`ls-tree` 109, `for-each-ref` 108, `log` 2. They come from four questions:

| Question | Asked by |
|---|---|
| Which directories does Git ignore entirely? | Go and TypeScript lint (`skipguard`, `docslinks`) |
| Which files does the index track? | TypeScript `build~types`, Python image, clientgen, Rust |
| Is this a work tree, with a commit, and is it shallow? | `validate~api`, `build~types`, the version stamp |
| What did the line's last release hold? | `validate~api` (`rev-parse`, `for-each-ref`, `ls-tree`, `cat-file`, `log` per project) |

The engine already reads two of them once per run: the index, for its
emptied outputs (`tooling/cli` ADR 0068), and each declaring project's
release baseline, for its key (ADR 0062). The ignored directories and the
repository state are new reads of the run, one each. A job that asks Git
pays a process, and its answer can differ from the one the engine keyed: a
cached verdict that read a fact its key does not hold is served to a tree
where the fact differs.

ADR 0071 settles that rule for the host RPC channel: a planned job gets no
channel, because its result is cached and a call its cache key does not cover
must not decide it. The context is the other way a fact reaches a job.

## Decision

1. **Four facts, each a runtime input.** A task declares the facts it reads as
   runtime inputs (`"from": "runtime"`):

   | Input | Fact |
   |---|---|
   | `ignoredDirectories` | The directories Git ignores entirely, at or below the task's directory |
   | `trackedFiles` | The paths the Git index tracks at or below the task's directory |
   | `repository` | The repository state: `unmanaged`, `no-commit`, `shallow` or `complete` |
   | `releaseBaseline` | The project's release baseline (ADR 0062): the state, the tag, and whether a commit since it declares a breaking change |
   | `releaseBaselineFiles` | Beside `releaseBaseline`, the directory that holds the project as the line's last tag holds it |

   The task's directory is its project's directory, or the workspace root for
   a workspace task. Paths are relative to the workspace root, in slash form,
   sorted and unique. A listing is read from the index or the work tree, never
   from a commit, so it names no commit, no branch and no checkout directory
   (ADR 0059).
2. **A job receives a fact only when its key holds it.** The CLI folds each
   declared fact into the task's key, then writes the same value to the
   context's `tree` member. A task that declares none gets no `tree`. A cached
   job therefore never reads a fact its key does not cover, which is ADR 0071
   point 1 applied to the context. An uncached task follows the same rule, so
   one declaration means one thing whatever the cache policy.
3. **History goes only to the tasks that declare it.** Only `releaseBaseline`
   reads history. The baseline names the tag, not a commit HEAD reaches, and
   whether a commit since the tag declares a breaking change (ADR 0062). The
   base version stays out of every key and every fact (ADR 0060).
4. **One read per run, before its first task.** The engine reads each
   work-tree fact a planned task declares once per run for the workspace,
   before the run's first task starts, and answers every task from that read:
   the index read of ADR 0068 gives the tracked files, one `ls-files --others
   --ignored --directory` gives the ignored directories, two `rev-parse` calls
   give the repository state, and each project's release baseline, which reads
   history no task of the run writes, is read once when its key or context
   first asks (ADR 0062). A work-tree fact describes the tree as the run found
   it: a directory a task of the run creates is not in it, whatever the order
   tasks run in, and the key holds the same value.
5. **A release baseline's files are extracted once per tag commit, for the
   tasks that ask for them.** A task that declares `releaseBaseline` gets the
   baseline its key already holds, at no cost beyond the key's read. Only a
   task that also declares `releaseBaselineFiles` gets the files. For each
   version line, the engine extracts the files the line's last tag holds at
   the directories of the run's projects that declare both, with one
   `ls-tree` and one `cat-file --batch`, into a directory of the run keyed by
   the tag's commit and the line. The files are the raw blobs the tag's tree
   names: no export attribute or filter applies, so they are the content the
   key's object names. Each directory is placed by the path Git names it by,
   so a project reached through a link is found, and one the extraction does
   not hold is an error. The context names the project's directory in it; it
   is absent when the project's directory is absent at the tag. The key holds
   the object the tag holds at the project directory, so the content of the
   directory is covered, and `releaseBaselineFiles` adds nothing to the key: a
   task that starts declaring it keeps the key's runtime identity. A job reads
   the directory and never writes to it; the run removes it when it ends.
6. **A failure is not a fact.** When Git fails, a missing git program or a
   refused repository included, the task's key fails and the task runs
   uncached (ADR 0062 point 2), and the context omits the fact. Only a
   workspace outside every repository is an answer: `unmanaged`, with empty
   listings. Extracting a release baseline's files is not a read of the key:
   the key holds the baseline, which was read, and the object the tag holds
   at the project directory, which names the files. An extraction that fails
   therefore keeps the key; the CLI logs the failure and the context omits
   `releaseBaseline` whole, so the job reads Git as it would without the
   member. That reading resolves the line's last release again, by the tag's
   name: a tag moved between the key's read and the job's stores a verdict of
   the new commit under the key of the old one, as every job that reads Git
   for its baseline does.
7. **The member is additive.** `tree` is an optional v2 member (ADR 0001). The
   SDK accessor answers from it when present and falls back to Git when it is
   absent, except for the release baseline, whose reader keeps its own Git
   reading (point 3). An older CLI resolves nothing for a runtime input it
   does not know (`TaskRuntimeIdentity`), so an extension built on the new SDK runs there as
   it runs today, asking Git. The names need no CLI contract rung: on an older
   CLI the key holds no more and no less than it does today.
8. **The files are those of the commit the baseline names.** The object the
   key holds and the files extracted are both read from the commit the line's
   last tag pointed at when the baseline was read, never through the tag's
   name again, so a tag moved during the run cannot pair a key with the
   context's files of another commit. A job that reads Git instead of its
   context (point 6) resolves the tag itself and is not covered.

## Rejected alternatives

- **Every fact to every job.** The tracked files of this repository are
  12,975 paths at `8e2cf03f`; writing them into 2,212 context files is the cost this
  decision removes, and a cached job would read what its key does not hold.
- **A fact without a declaration, keyed through the task's file patterns.** A
  `git:` pattern keys the candidates, not which of them the index tracks, and
  an ordinary pattern keys neither. The key would cover a fact by accident.
- **The whole workspace's listing in every key.** A file added anywhere would
  move the key of every task that declares the fact. The listing is bounded to
  the task's directory.
- **A CLI contract rung, as ADR 0062 point 4 has.** `releaseBaseline` made an
  uncached task cached, so an older CLI that keyed it without the baseline
  would have served a stale verdict. These facts make no task cached that was
  not: an older CLI keeps today's keys and today's Git calls.
- **A host RPC call per fact.** ADR 0071 refuses a planned job a channel.
- **The release baseline read by each job.** Projects of one line share a tag;
  one extraction per tag commit replaces a `ls-tree` and a `cat-file` per
  project.

## Consequences

- A job that declares a fact and reads it from its context starts no Git.
  The jobs keep their own Git reads until each moves onto the accessors.
- `ignoredDirectories` lists the ignored directories present when the run
  starts, not Git's ignore rules. A question about a path that does not exist
  yet, such as `check-ignore --no-index` on an output directory, and history
  reads other than the release baseline, such as a `merge-base` or a `show`,
  are not facts of this decision.
- Declaring `trackedFiles` or `ignoredDirectories` moves a task's key when a
  file under its directory is added to or removed from the index, or when a
  directory under it becomes ignored.
- A task that reads the workspace's facts from a project task, such as a link
  check across projects, is not covered by this decision: its directory is its
  project's.
- Removing a fact or a runtime input name is breaking.
