# ADR 0060 — The base version is not a cache key input

- **Status**: accepted
- **Scope**: `@putnami/cli` (`internal/store`, `internal/jobs`, `internal/git`),
  `@putnami/go` (`build-compile`, `build-cross-compile`, config schemas)
- **Amends**: [ADR 0059](0059-a-cache-key-describes-the-tree-not-the-commit.md)

## Context

ADR 0059 removed the commit from every key and kept one history-derived
field: `WorkspaceVersion`, the base version of the project's release line. Git
derives it from the line's last tag and the conventional commits since that
tag. Two checkouts of one tree resolve the same base only when they hold the
same tags and the same commit messages, and lanes do not:

- The hosted pull request runner tests a squash commit it builds on the base
  commit. The commit is dated at the Unix epoch so that one base and one tree
  always give one commit. The clone is 64 commits deep and fetches no tags.
- The `main` lane publishes, so it fetches every tag.
- A squash title can call for another bump than the branch's commits.

The CLI also refused a commit time of 0. On the pull request lane it computed
no version at all, so every key of that lane carried an empty base version.
Four consecutive pull request runs shared none of their keys with the `main`
runs that followed their merges, while branch-push lanes on the same trees
shared 93 % to 98 %.

## Decision

1. **No key reads the base version.** `CacheKey.WorkspaceVersion` is removed.
   A version reaches a key only through `EmbeddedVersion`, which a task opts
   into with `cache.versionAware` or a `version-var` parameter. A task whose
   output embeds the base version declares it the same way.
2. **A commit dated at the epoch has a time.** Git records it as 0, and the
   version suffix reads `19700101000000-<sha>`.
3. **The key format version becomes `v9`.** Removing a field moves every key.
   The bump keeps an old entry from ever answering at a new address.
4. **A cached task reads a line's version only when its key carries it.** The
   job context of a task the cache can serve gives base version `0.0.0` and no
   commit, in its own version, its workspace block and every project
   reference, unless the task declares `cache.versionAware` or reads a set
   `version-var`. A task the cache never serves reads the version Git
   resolved. The rule reads the task, not `--no-cache`.
5. **The build stamp names the tree's version.** Every
   `capabilityPackages[].version` in `.gen/version.json` is `0.0.0`. It equals
   the version the project references of a cached task carry, which SDD
   evidence matching compares. The v2 capability manifest drops it.
6. **The shipped extensions write no line version into a cached output.** Go
   `build-compile` and `build-cross-compile` no longer write a `VERSION` file;
   nothing read it. Every Go config schema, committed or under `.gen`, carries
   the version the project declares in its `putnami.json`, or `0.0.0`.

## Consequences

- Two runs on one tree share every key that is not version-aware and copies no
  stamp through a generate asset, whatever the commit, the branch, the
  checkout directory, the tags the checkout holds or the base version its
  history resolves.
- The rule holds by construction: a cached task cannot read a version its key
  does not carry. A task that needs the released version declares
  `versionAware`, and its key then moves with every commit. A packaging task
  that forgets reads an empty full version and fails instead of publishing
  `0.0.0`.
- The stamp's commit fields (`version`, `suffix`, `sha`, `branch`,
  `buildTime`) stay readable by every task, under the rule of ADR 0059. A page
  prerendered from them is served at another commit's identity on a hit.
- Every cache entry misses once after the upgrade. The selection fingerprint
  of every release-set member moves once, so the next publication republishes
  every member once.
