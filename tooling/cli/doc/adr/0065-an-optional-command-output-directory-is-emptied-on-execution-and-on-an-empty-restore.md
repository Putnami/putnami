# ADR 0065 — An optional command-output directory is emptied on execution and on an empty restore

- **Status**: accepted
- **Scope**: `@putnami/cli` (`internal/jobs`, `internal/store`)
- **Amends**: [ADR 0064](0064-an-executing-task-starts-from-an-empty-owned-directory.md),
  points 4, 7 and 8 and its `optionalEmpty` consequence

## Context

ADR 0064 empties a required directory output before its task executes and
leaves an `optionalEmpty` one as it stands. The command-output directories of
the compile, cross-compile, transpile, types and package steps are all
`optionalEmpty`, so they keep every file an earlier run left. The Go
`build-cross-compile` writes one binary per target into `bin`. After a project
drops a target, the next execution leaves the old binary in `bin`, the capture
stores it under the key of the new sources, and a later restore brings it back.
A packager or a publisher then ships a platform the project no longer builds.

ADR 0064 left optional outputs alone because of the restore. A run that writes
nothing records the output empty, and a restore of an empty record leaves the
path as it stands, so an execution that emptied the path would leave another
tree than the restore. That reason holds for a project or workspace path, where
an optional output can hold a file no task writes, such as
`.gen/clientgen/config.json`. It does not hold for a command-output directory.
The engine creates `.putnami/out/<project>/<command>/` for the command,
`validatePlanContract` gives each output one owner in a plan, and a task cedes
a subpath that another task writes there with `excludes`. The output holds
what its owner writes and the subpaths the owner cedes or preserves, and
nothing else.

## Decision

1. **Before a task executes, the scheduler also empties each `optionalEmpty`
   directory output whose effective root is the command-output directory and
   whose path the declaration states.** The output is not `pathFrom`, not
   invocation-scoped, and its path normalizes. The emptying is the one ADR 0064
   defines: the same keep set, the same failure, and the same place in
   `prepareTask`.
2. **An optional project or workspace output is still not emptied.** ADR 0064
   point 7 holds for those roots.
3. **A restore of an entry that records such an output empty empties it.**
   `materializeDeclaredOutputs` calls `emptyDeclaredOutput`, the call an
   execution makes, with the same keep set, so a restore leaves the tree the
   producing execution left. Every restore path takes it: a local hit, a remote
   hit and a coalesced waiter all restore through `restoreDeclaredCacheHit`. A
   status-only hit
   ([ADR 0058](0058-a-hit-no-job-reads-is-restored-without-its-files.md))
   writes nothing to the workspace and empties nothing. The command-output
   detach runs before the emptying, so no rename reaches through a link an
   earlier session left.
4. **The store does not decide.** `MaterializeTaskOutput` still never touches
   the destination of an empty record. Only the engine reads the declaration,
   so the engine empties.
5. **A failure to empty on restore is a restore failure.** The hit is not
   served. The run does what it does after any failed restore: the
   claim-or-wait path restores a published entry once more, and the task
   executes when no restore succeeds. The execution empties again, or fails
   the task as ADR 0064 point 6 states.
6. **One predicate decides.** `emptiedOutputDeclarations` selects what an
   execution empties, what a restore of an empty record empties, and the ids
   the key names (`store.CacheKey.EmptiedOutputs`, ADR 0064 point 9). The
   format version stays `v9`: the new ids move keys only to addresses no
   earlier entry occupies.

With this decision, points 7 and 8 of ADR 0064 read as follows. A required
directory output and an `optionalEmpty` command-output directory output are
emptied. A file output, an invocation-scoped output, a `pathFrom` output and
an optional project or workspace output are not. Point 4 holds for every
restore except one of an empty record, which point 3 above covers: a cache hit
and a coalesced waiter that restore such a record empty the output.

## Consequences

- From the same inputs, an execution and a restore leave the same
  command-output directory. A binary for a dropped target leaves `bin` at the
  next execution and never enters an entry.
- Each task with a newly emptied output misses once after the upgrade, and so
  does each task whose key folds one of their hashes. In this repository those
  are the Go `build-compile` and `build-cross-compile` (`bin`), `package-go`,
  `package-archives` and `package-docker`; the TypeScript `build-transpile`
  (`lib`), `build-types` (`types`), `build-compile` (`compile`), `package-npm`
  and `package-docker`; and the scaffold `package-content` (`archives`). The
  selection fingerprint of each release-set member whose key moved moves once,
  so the next publication republishes those members once.
- A packager that runs and writes nothing now leaves its directory empty, so
  publish no longer finds the channel record an earlier run left. An image
  packager whose base packager wrote nothing fails instead of building from an
  earlier layout.
- A reader in another session sees a command-output directory empty from the
  emptying until the task writes it again, and during a restore of an empty
  record. A session that runs the same task with other parameters can also
  replace the directory's contents. The task-output locks cover one task, not
  the gap between a producer and a reader of the same command, so that reader,
  such as `package-archives` reading `bin`, can see the other session's set.
  Before this decision the other session overwrote only the files it wrote. ADR
  0064 already accepts this risk for required outputs, and this decision
  extends it to the binary and package directories.
- No drift verdict changes. `validateOutputDrift` refuses a drift policy on the
  command-output root, so no emptied command-output directory is policed.
- An optional project or workspace output and every `pathFrom` output keep a
  file their task stops writing. Their tasks own that removal. In this
  repository that covers the four Go `build-describe` outputs and the generated
  client directories.
