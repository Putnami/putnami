# ADR 0003 — A declared directory output may cede a subpath to the task that produces it

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/extension` (`protocols/extension`), the
  `excludes` member of a declared output, and the CLI's declared capture,
  restore and plan-time ownership checks

## Context

One owner per output: no two tasks declare the same path, and file-vs-subtree
nesting counts as the same path. One shape breaks that rule: a tree whose
owner runs first and whose subtree a later task writes. `@putnami/go`
`build-generate` declares `<project>/.gen` and runs first; `build-describe`
runs later and writes `<project>/.gen/migration-bundle`, the path migration
publication, deploy, `database.ApplyBundle` and the database test provider
read. If generate's entry captures and restores `.gen` whole, a run served
from cache loses the bundle.

## Decision

1. **A directory output may cede named subpaths.** `excludes` lists literal
   root-relative paths, each strictly inside the output's `path` and never
   equal to it. The ceding task neither captures them nor replaces them on
   restore, and another task may declare them.
2. **The carve-out is part of the declaration.** It rides on `OutputRef`, so
   the manifest-local ownership check and the planner's resolved-ref check use
   one predicate (`OutputsOverlap`).
3. **A cede is decidable from the declaration alone.** Only a `directory`
   output with a literal `path` may carry one: a file has no subpath, and a
   `pathFrom` value is unknown until the task runs. Entries normalize and
   sort. A non-directory output, a `pathFrom` path, an entry outside or equal
   to `path`, and a duplicate are validation errors. A malformed entry never
   reaches the ownership comparison, so a defective declaration cannot buy an
   exemption.
4. **Restore leaves a ceded subpath as it finds it.** Declared restore carries
   whatever the destination holds there across the directory swap, as an
   execution of the ceding task does. Commands of one session interleave, so
   removing and later restoring the subtree leaves a window where another
   command reads an empty contract path.
5. **Nothing is added to the cache entry.** Ceded bytes are never staged. The
   cache key folds the task-contract digest, so gaining or losing a carve-out
   moves the key, and restore reads the carve-outs from the current
   declaration, which the key proves matches the entry's.

Invariants:

- A ceded subpath has exactly one owner, never the ceding task; a second
  claimant still collides with the first.
- Ceding a subpath cedes neither its parent nor its siblings.
- A ceded subpath no task claims is captured by nobody, by declaration.
- `excludes` is not an escape hatch for undeclared writes: a region nobody
  declares stays undeclared.

## Rejected alternatives

- **Declare the subpath from the later task without a cede.** The planner
  rejects the overlap, correctly: the owner's capture would swallow it.
- **Stage a private copy under the task's command output.** The bundle is
  cached but the contract path stays empty, so every consumer learns a second
  location.
- **One declaration per subpath of `.gen`.** An open-ended generated tree
  becomes a manifest every new generator edits.
- **Record the carve-out in the cache entry.** Nothing would read it.
- **Allow a glob.** A set of paths cannot have a single owner, the same reason
  `path` is glob-free.

## Consequences

- The owning task must be scheduled after the ceding task in every command
  that schedules both, so the ceded path reflects the current sources.
  `@putnami/go` pins that order in its manifest test.
- The extension SDK authoring helpers expose no option for `excludes`; a
  Go-authored manifest sets it on the struct.
