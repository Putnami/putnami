# ADR 0059 — A cache key describes the tree, not the commit

- **Status**: accepted, amended by
  [ADR 0060](0060-the-base-version-is-not-a-cache-key-input.md)
- **Scope**: `@putnami/cli` (`internal/store`, `internal/jobs`)

## Context

A squash merge puts a new commit on the main branch over a tree its pull
request already built and tested. A run on that commit reads the same files as
the last run of the pull request. It differs in the commit, the branch and,
on most runners, the checkout directory.

A task key that reads any of the three misses on that run, and the miss
reaches every downstream key through the upstream fold. Two inputs can carry
them into a key:

- `<project>/.gen/version.json`, the build stamp, records the commit a project
  is built at in its `version`, `suffix`, `sha`, `branch` and `isDirty`
  fields. A Docker publish merges `image`, `image_digest` and `publish` into
  it, and the scheduler carries them into the stamp of the next commit. The
  scheduler writes the stamp before any key is computed, and a task whose
  input globs reach it, such as TypeScript lint's `**/*.json`, hashes it.
- A generate asset (`options.generate.assets[].from`) is an input of every
  task of the project that declares it. The scheduler resolves it to an
  absolute path.

## Decision

A file-content input describes the tree. Apart from a stamp that a generate
asset copies, the commit reaches a key only through the publish version, which
a task opts into.

1. **A file pattern hashes the stamp as a description of the tree.** The
   digest leaves out these top-level fields, and no other:
   - `buildTime`, which names the invocation.
   - `version`, `suffix`, `sha`, `branch` and `isDirty`, which name the commit.
   - `image`, `image_digest` and `publish`, which name a publication of the
     commit.

   The digest keeps every other field, whoever wrote it: `name`,
   `capabilityRoot`, `capabilityPackages`, `contentHash`, and any field the
   list above does not name.
2. **An asset under the workspace root is named by its workspace-relative
   path.** The path is in slash form, so the checkout directory and the host
   separator do not reach the key. The asset's content and its place in the
   workspace do. An asset outside the workspace root is named by its absolute
   path. A build stamp inside an asset directory keeps every field but
   `buildTime`: the asset copies the stamp into the task's output, so the key
   follows the commit and the publication that copy names.
3. **A task whose output embeds the commit declares it.** `cache.versionAware`,
   or a `version-var` parameter the task receives, puts the publish version in
   the key. Listing the stamp as an input does not.
4. **The key format version stays `v8`.** A key that read the stamp or an
   asset moves to a new address, where no earlier entry lives. A key that read
   neither keeps its address and its meaning. The entries left behind age out
   through garbage collection.

### Why the digest, and not the input globs

Excluding `.gen/version.json` from each extension's patterns would fix the
patterns this repository ships and no other. The digest applies wherever a
file pattern reads the stamp: a project glob or a closure pattern.

### Why Git candidate inputs stay raw

A `git:` pattern hashes the raw bytes of each candidate
([ADR 0041](0041-git-candidate-file-inputs.md)), because a repository scanner
reads those bytes. The stamp is a candidate only where Git tracks it or does
not ignore it.

## Consequences

- Two runs on one tree, at one base version, share every key that is not
  version-aware and copies no stamp through a generate asset, whatever the
  commit, the branch or the checkout directory.
- A key still carried the base version of the project's release line
  (`WorkspaceVersion`), so two lanes that resolved two base versions on one
  tree keyed differently. [ADR 0060](0060-the-base-version-is-not-a-cache-key-input.md)
  removes it.
- A task that reads the commit from the stamp, without declaring
  `versionAware` or receiving `version-var`, is served the output of another
  commit on a hit.
- A hit never restores an output whose copied stamp names another commit: a
  stamp inside a generate asset keeps its commit fields in the key.
- A cache hit that restores a `.gen` subtree still leaves the stamp naming the
  current commit: the scheduler rewrites the stamp after the restore.
- The selection fingerprint of a release-set member whose package key reads
  the stamp or a generate asset moves once. The next publication republishes
  that member once.
