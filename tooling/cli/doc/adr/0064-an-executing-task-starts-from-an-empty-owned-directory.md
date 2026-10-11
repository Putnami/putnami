# ADR 0064 — An executing task starts from an empty owned directory

- **Status**: accepted
- **Scope**: `@putnami/cli` (`internal/jobs`, `internal/store`)

## Context

The engine captures a declared directory output as it stands after the run.
The task runs in the worktree, not in a sandbox, so the directory still holds
what earlier runs wrote. A generator that stops emitting a file leaves that
file in place: after a page or the last island of a `@putnami/web` site is
deleted, `build-generate` keeps the page's prerendered HTML and the island
bundles in `.gen/public`. The capture stored those files under the key of the
new sources, and a later build from a deleted `.gen` restored them (issue
#149). The output of a task marked `deterministic` depended on what the
directory held before the run, which no key describes.

A restore already behaved the other way. It replaces what the task owns,
removes each owned entry the entry does not hold, and leaves the ceded
(`excludes`) and preserved (`preserves`) subpaths alone. From the same inputs,
an execution and a restore left two different trees.

A fix in each generator leaves every other directory writer with the same
defect, and the engine cannot detect one that forgets. The engine owns the
declaration, so it can give every task the same starting point.

## Decision

1. **Before a task executes, the scheduler empties each durable directory
   output whose path the declaration states.** It removes every entry the task
   owns there and keeps the subpaths a restore keeps: the ceded and preserved
   ones, resolved by `outputSkippedSubpaths`, the one resolution capture,
   restore and drift already share. The output directory stays, with its mode.
   An absent output stays absent. A file or a symbolic link at the path is
   removed without being followed.
2. **The build stamp stays.** `.gen/version.json` belongs to the scheduler.
   `refreshVersionFile` writes it once per project and session, in the
   preparation of the first task that executes. Emptying it for a later owner
   of `.gen` in the same session would start that owner without a stamp and
   capture none. The emptying keeps the stamp in any project-rooted output
   that holds it, and keeps the whole document, so the fields other writers
   merged into it survive.
3. **The emptying runs in `prepareTask`**, after the drift reference and the
   command-output detach, and before the stamp refresh, the `preBuild` hook and
   the subprocess. Each retry empties again before it runs.
4. **Only an execution empties.** A cache hit, a follower of a plan-level shared
   node (`sharedResult`) and a coalesced waiter execute nothing, so they empty
   nothing. A follower captures the tree its leader wrote. Each member of a
   batch is emptied in its own preparation, before the one subprocess starts.
5. **The emptying is the pruning half of a restore** (`store.EmptyTaskOutput`
   over `pruneAround`). Each owned entry leaves the output in one rename into a
   private directory and is reclaimed from there, so a reader finds an entry
   whole or gone. The private directory is created in the workspace scratch
   root, beside the output when a rename cannot reach the scratch root, and
   inside the output when the output is a mount of its own. On Windows, a rename
   that fails because another process holds a handle is waited out, as a restore
   waits it out.
6. **A failure to empty fails the task.** `prepareTask` returns a failed row
   whose error names the output. The subprocess never starts and nothing is
   captured. A retry that cannot be given an empty directory is not attempted,
   and the task keeps the failure it already has.
7. **Three outputs are not emptied.** A file output, which the task replaces
   whole. An invocation-scoped output, which lives in a private tree created
   for the run. A `pathFrom` output, whose path is known only after the run. A
   literal path never names its root (`extension.NormalizeOutputPath`), so the
   project root, the workspace root and a command-output directory are never
   emptied.
8. **The key names the emptied outputs.** `store.CacheKey.EmptiedOutputs`
   holds their ids. The hash writes `emptiedOutputs` and the sorted ids after
   the source-state block, and writes nothing when the list is empty. An entry
   stored by a CLI that emptied nothing therefore sits at an address no key of
   this CLI computes for a task it empties. The format version stays `v9`:
   the change moves keys only to addresses no earlier entry occupies, which is
   the rule [ADR 0063](0063-a-git-input-keys-the-executable-bit.md) applies.
   The selection key carries the marker too: a publication recorded under an
   engine that emptied nothing is not known to hold what this engine builds.

## Consequences

- From the same inputs, an execution and a restore leave the same tree, and an
  entry holds only what its run wrote. Deleting a page or the last island
  removes their output on the next build, and no later restore brings it back.
- Every task with an emptied output misses once after the upgrade, and so does
  every task whose key folds one of their hashes. A task with no emptied output,
  such as a lint or test task that declares none, keeps its address.
- The selection fingerprint of each release-set member whose key moved moves
  once, so the next publication republishes those members once, as after
  [ADR 0060](0060-the-base-version-is-not-a-cache-key-input.md).
- A file another task writes inside an owned region goes at the owner's next
  execution, as the owner's restore already removed it. The `config-extract`
  fallback `.gen/config-schema.json`, written when
  `options.generate.schema=false`, is one: an executing `build-generate` removes
  it. A path that must outlive the owner's execution has to be ceded by the
  owner and declared by its writer.
- A `pathFrom` output keeps a file its generator stops emitting, in the
  worktree and in the entries captured from it. The generator owns that
  removal. Every drift-policed output in this repository is a `pathFrom`
  output, so no drift verdict changes.
- A restore of an output recorded empty leaves the path as it is
  ([ADR 0034](0034-a-declared-output-polices-its-own-drift.md)), while an
  execution empties it. The Go `build-generate` writes static contracts into
  `.gen/schema`, which it cedes to `build-describe`. For a project with no
  describe phase, an executing `build-describe` that skips now empties
  `.gen/schema`, and a replayed skip leaves it. Each reader of those contracts
  falls back to the committed `schema/` copy.
- A reader in another session, such as a development server, sees an owned
  directory empty from the emptying until the task writes it again. The
  task-output locks order writers across sessions, not readers.
- A failed execution leaves only what it wrote before it failed.
- A scheduler-level `preBuild` hook runs once per extension and project per
  session. A later executing task that owns the directory the hook wrote
  removes the hook's files. No extension in this repository declares both a
  scheduler-level hook and a task: the framework hooks run inside the
  TypeScript `build-generate`, after its emptying.
- The TypeScript `build --clear` parameter stays. It removes more than the
  emptying does: the ceded files and the stamp.
