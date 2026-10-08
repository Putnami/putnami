# ADR 0059 — A cache key describes the tree, not the commit

- **Status**: accepted
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
  fields. The scheduler writes it before any key is computed, and a task whose
  input globs reach it, such as TypeScript lint's `**/*.json`, hashes it.
- A generate asset (`options.generate.assets[].from`) is an input of every
  task of the project that declares it. The scheduler resolves it to an
  absolute path.

## Decision

A file-content input describes the tree. The commit reaches a key only through
the publish version, which a task opts into.

1. **The stamp is hashed as a description of the tree.** Its digest leaves out
   `buildTime`, which names the invocation, and `version`, `suffix`, `sha`,
   `branch` and `isDirty`, which name the commit. Every other field stays in
   the digest, whoever wrote it: `name`, `capabilityRoot`,
   `capabilityPackages`, and the fields other writers merge into the document,
   such as `contentHash` and `publish`.
2. **An asset under the workspace root is named by its workspace-relative
   path.** The path is in slash form, so the checkout directory and the host
   separator do not reach the key. The asset's content and its place in the
   workspace do. An asset outside the workspace root is named by its absolute
   path.
3. **A task whose output embeds the commit declares it.** `cache.versionAware`,
   or a `version-var` parameter the task receives, puts the publish version in
   the key. Listing the stamp as an input does not.
4. **The key format version stays `v8`.** A key that read the stamp or an
   asset moves to a new address, where no earlier entry lives. A key that read
   neither keeps its address and its meaning. The entries left behind age out
   through garbage collection.

### Why the digest, and not the input globs

Excluding `.gen/version.json` from each extension's patterns would fix the
patterns this repository ships and no other. The digest applies wherever the
stamp is read as a file-content input: a project glob, a closure pattern, and
a directory asset that holds another project's stamp.

### Why Git candidate inputs stay raw

A `git:` pattern hashes the raw bytes of each candidate
([ADR 0041](0041-git-candidate-file-inputs.md)), because a repository scanner
reads those bytes. The stamp is a candidate only where Git tracks it or does
not ignore it.

## Consequences

- Two runs on one tree, at one base version, share every key that is not
  version-aware, whatever the commit, the branch or the checkout directory.
- A key still carries the base version of the project's release line, which
  comes from tags and history. Two commits on one tree that resolve two base
  versions key differently.
- A task that reads the commit from the stamp, without declaring
  `versionAware` or receiving `version-var`, is served the output of another
  commit on a hit.
- A cache hit that restores a `.gen` subtree still leaves the stamp naming the
  current commit: the scheduler rewrites the stamp after the restore.
- The selection fingerprint of a release-set member whose package key reads
  the stamp or a generate asset moves once. The next publication republishes
  that member once.
