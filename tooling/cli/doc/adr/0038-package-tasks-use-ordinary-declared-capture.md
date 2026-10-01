# ADR 0038 — Package tasks use ordinary declared capture

- **Status**: accepted
- **Scope**: `tooling/cli-model` (`UsesDeclaredCapture`), `tooling/cli`
  (`--plan`), `tooling/extension-sdk/pkgmeta`, packager manifests

## Context

The tasks the `package` pipeline schedules include `build-tidy`,
`build-generate`, `build-describe` and `build-cross-compile`, the same tasks
`build` schedules and caches. They are a large share of a release run's task
time, and they must restore under `package` exactly as under `build`.

A cache hit can only be correct when the task writes nothing outside its
declared outputs. A shared channel index that several packagers
read-merge-write has no single owner, so no task can declare it. A hit would
restore the artifact and skip the merge, and `publish` would then see an
unrecorded channel and silently skip the publication.

## Decision

1. **Each packager records its channel inside its own output.**
   `pkgmeta.WriteChannelRecord` writes `<its output directory>/channel.json`.
   The channel index is derived over those records (`pkgmeta.ReadChannelIndex`)
   and is never a file. The record travels with the artifact it describes, so a
   restore of `go/` restores "the go channel was packaged at version X"
   atomically with the module zip.
2. **Declared capture is the declaration, in every command.**
   `UsesDeclaredCapture(job)` is `TaskDeclarationOf(job) != nil`. No command
   has a rule of its own. Output ownership stays enforced where it is decided:
   `ValidateOutputOwnership` in the manifest and `validatePlanContract` across
   the plan.
3. **`--plan` prints `NO-CACHE`** for any job `CanUseCache` rejects, so a task
   that never consults the cache is not rendered as a `MISS`.
4. **`<command-output>/metadata.json` is the archive publication manifest,
   with one owner.** An archive uploader outside this repository reads its
   `version`, `artifact`, `stable`, `channels` and `template` fields, and runs
   with `--if-present`, so a missing file would silently skip every archive
   publication. The file is declared as a `command-output` file by the one task
   that writes it: `package-archives` in the Go extension or `package-content`
   in scaffold (a project activates at most one). The task writes the whole
   document rather than merging into it, and a cache restore reproduces it.
5. **A packager whose channel record fails to write fails the step.** An
   unrecorded channel is a publication that silently does not happen.
6. **`cache.restoreMode` is retained, deprecated and ignored.** `ParseManifest`
   rejects unknown fields, and published manifests carry it, so deleting it
   would make a newer CLI refuse older extensions.
   `TestNoPackageTaskDeclaresARestoreMode` keeps in-tree manifests from setting
   it. It is removed when no pinned manifest declares it.

## Consequences

- Same tree, second session (a retry, a `publish` after a `package`, a
  finalizer gate after a worker gate): every `package~*` task restores.
- New commit, unchanged sources: `package~tidy`, `package~generate` and
  `package~describe` restore; their keys carry no version.
  `package~cross-compile` restores for projects without a `version-var`. The
  `versionAware` packagers (`package-go`, `package-archives`, `package-docker`,
  `package-npm`, `package-content`) miss by design.
- A `--dry-run` package writes its channel records and the archive publication
  manifest. It cannot poison a real run's entry: `dry-run` is a flag of the
  `package` command, and command flags are folded into every one of its tasks'
  keys.
- The TypeScript extension has no archive channel and writes no
  `metadata.json`. An archive uploader pointed at a TypeScript project skips
  under `--if-present`.

## Alternatives rejected

- **A `package~record` merge step.** It needs a `dependsOn` on every packager
  in every extension, stays uncacheable, and re-creates a single-owner file the
  per-output records already replace.
- **Declare `metadata.json` as an output of each packager.** Several owners of
  one path is the invariant violation, not a way around it.
- **Delete `metadata.json` and move the external reader to
  `archives/channel.json` at once.** It needs a cross-repository release and
  opens a window in which archive publication silently skips.
- **Keep a package exception opted into with `restoreMode`.** The opt-in
  asserts a shape, not the property that matters: `build-cross-compile`
  declares two outputs and `build-describe` six, both completely declared, and
  neither could make the assertion.
