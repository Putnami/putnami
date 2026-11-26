# ADR 0107 — An emptied file output is removed on execution and on an empty restore

- **Status**: accepted
- **Scope**: `@putnami/cli` (`internal/jobs`), `@putnami/go` and
  `@putnami/typescript` (`build-infra`, `package-deployment`),
  `go.putnami.dev/sdk/extension/infraagg`
- **Amends**: [ADR 0068](0068-an-emptied-optional-project-output-is-emptied-on-execution-and-on-an-empty-restore.md)
  decisions 1 and 2

## Context

`build-infra` aggregates a workload's deployability requirements into
`.gen/requirements.json`. It skips a library, and a workload that declares
nothing removes an earlier manifest and reports SKIP. The output was
required, so such a skip cached nothing, by design: a hit must never restore a
manifest the current inputs do not produce. The cost is one extension process
per library on every run of an unchanged workspace. This repository has dozens
of Go libraries, and the count test named `/lib:build~infra` among the tasks
that execute every time. `package-deployment` declares `.gen/deployment.json`
the same way.

An optionalEmpty output would cache the skip, but a restore of its empty
record leaves the path as it stands, so a manifest an earlier run wrote would
survive the hit.

## Decision

1. **A declared file output can ask to be emptied.**
   [Protocol ADR 0021](../../../../protocols/extension/doc/adr/0021-an-emptied-file-output-is-removed.md)
   extends `emptied` to an `optionalEmpty` durable file output with a literal
   path under the project or workspace root, under CLI contract 27.
2. **The scheduler removes such a file** before its task executes and on a
   restore of an entry that records it absent. `EmptiedOutputDeclarations`
   admits it, so the execution, the restore of an absent record
   (`emptiedByRestore`) and the key (`emptiedOutputIDs`) still follow one
   predicate. A symlink at the path is removed without following it. A
   directory at the path is no file the task wrote: the removal fails, which
   fails the execution (ADR 0064 point 6) or the restore (ADR 0065 point 5).
   ADR 0068 decision 6 applies to a file at the path: a file the Git index
   tracks there refuses the removal and the restore, naming the `git mv` to
   run. An emptied file absent from the worktree is not asked about, because
   removing it removes nothing and restoring it overwrites nothing, so a
   library's `build-infra`, which writes no manifest, starts no git. A
   tracked file already deleted from the worktree there is not reported,
   where an emptied directory reports it with the `git rm --cached` to run.
3. **`build-infra` and `package-deployment` declare their files emptied.**
   `.gen/requirements.json`, `.gen/infra/runtime.json` and
   `.gen/deployment.json` become optionalEmpty and emptied in `@putnami/go`
   and `@putnami/typescript`. Their skips are deterministic: a library, a
   workload that declares nothing and a withheld aggregate follow from the
   files the key folds.
4. **A failed write or removal fails `build-infra`.** It reported SKIP, which
   cached nothing while the manifest was required. A cached SKIP now stands
   for the file's absence, so a run whose write failed must not store one.
   `infraagg.Job` reports FAILED with the error, as `infraagg.DeploymentJob`
   already did. Findings stay warnings.

## Consequences

- A second run of an unchanged workspace executes no `build-infra` or
  `package-deployment` on a library: the skip is served from the cache, and
  the restore removes a manifest an earlier run left.
- An execution and a restore from the same key leave the same file or its
  absence, so a deployer never reads a manifest the current inputs do not
  produce.
- The `.gen/infra/runtime.json` an earlier build left for a workload that now
  authors `infra/runtime.json` is removed by a hit too; before, a hit left it
  until the task executed again.
- Keys move once: the task contracts and the emptied output ids of
  `build-infra` and `package-deployment` change. The store format stays `v9`
  (ADR 0063).
- The Go and TypeScript extensions are stamped 27, so an older CLI refuses
  them with the contract error. The lock pins the CLI with them.
- A disk that refuses the manifest's write now fails the build, where it was
  a warning. The failure is recorded at its key (ADR 0090), so an unchanged
  workspace replays it until an input changes or `--retry-failed` runs it
  again, where the earlier SKIP ran again on its own. The protocol has no
  failure code for a transient condition of the host that would keep it out
  of the record, and `host-unsupported` states something else.
- A hit that restores only the status (a remote result-only hit, which writes
  no file) removes nothing, as it writes nothing for any task. A workload that
  now declares nothing keeps a stale manifest until a hit restores the files
  or the task executes.
- A file the run wrote while an earlier one was absent is never compared with
  the index: only the removal asks.
