# ADR 0014 — A TypeScript job reads the tree from its context

- **Status**: accepted
- **Scope**: `@putnami/typescript` (`typescript/extension`): the `build-tsc`,
  `lint-all`, `lint-check` and `validate-api` tasks
- **Applies**: [protocols/job ADR 0002](../../../../protocols/job/doc/adr/0002-a-job-reads-the-tree-facts-its-key-holds.md);
  keeps [tooling/cli ADR 0062](../../../../tooling/cli/doc/adr/0062-a-release-baseline-input-names-the-baseline-not-the-commit.md)

## Context

A TypeScript job asked Git what the CLI reads once per run. Measured on the
TypeScript case of the extension counts test
(`tooling/cli/internal/cli/e2e/orchestration`, `lint,test,build --all` on one
library, every task executed), the jobs made 7 Git calls:

| Task | Calls |
|---|---|
| `build-tsc` | 5: `rev-parse`, one `ls-files` and one `check-ignore` per output (`dist`, `tsconfig.tsbuildinfo`) |
| `lint-check` (skip guard) | 1: `ls-files --others --ignored --directory` |
| `lint-docs` | 1: `ls-files --cached --others --exclude-standard` of the workspace, which the SDK's docs lint reads from the context since protocols/job ADR 0003 |

`validate-api`, on a library whose export was removed under a `feat!:`
commit, made 12: `rev-parse` 3, `for-each-ref` 2, `ls-tree` 2, `cat-file` 3,
`ls-files` 1, `log` 1.

## Decision

1. **`build-tsc` declares `trackedFiles`.** The files the index tracks under
   the project come from the context. Whether Git ignores each output stays a
   Git question, asked once for every output with
   `git check-ignore --no-index --stdin`; that call's refusal outside every
   repository, or without a git program, is how the check tells it has
   nothing to check. A project inside a repository nested in the
   workspace's, a submodule or a nested clone, has its files tracked by that
   repository's index, which the context's listing does not read: for it the
   check lists the tracked files with Git.
2. **The skip guard reads what the lint's key holds.** `lint-all` and
   `lint-check` key their sources with ordinary patterns, whose file walk
   skips `node_modules`, `.git`, `.putnami`, `out`, `dist` and `vendor` and
   follows no ignore rule. The guard skips those names, dot directories and
   nested projects, and no longer skips the directories git ignores: it reads
   a subset of the key's files, its verdict depends on nothing else, and it
   starts no git. The tasks declare no tree fact.
3. **`validate-api` declares `releaseBaselineFiles` and `candidateFiles`.**
   The repository state, the line's last release, whether a commit since it
   declares a breaking change, and the files the tag holds at the project come
   from the context; `releaseBaselineFiles` adds nothing to the key (ADR
   0062). The working tree's files are the project's candidate cut
   (protocols/job ADR 0003), which `candidateFiles` adds to the key. Git is
   still read when the context does not hold the tag's files of the project:
   a package that moved since its release, a project directory the tag does
   not hold or that holds no `package.json` there, and a failed extraction.
   A project in a nested repository lists its working tree with Git, as
   build-tsc does (`treefacts.NestedRepository`).
4. **The version fallback is unchanged.** `package-npm` and `package-docker`
   read the version the CLI stamps in the context; Git is read only when a
   direct invocation carries none.

Measured after the change, with the docs lint reading the workspace's cut
from its context (protocols/job ADR 0003): the TypeScript case makes 1 Git
call from jobs, the `check-ignore`, and the engine 1 more than without this
change, its one read per run of the tracked files (`ls-files`). Removing the build's
`dist` runs no lint again. `validate-api`
makes none.

## Rejected alternatives

- **The lint tasks declare `ignoredDirectories`, and the guard skips the
  directories it lists.** The fact keys every ignored directory under the
  project, so building it, which fills `dist`, moved the lint's key: measured
  on the counts case, removing `dist` ran `lint~check` again where it was
  cached, and a built checkout and a fresh one, a developer's and CI's, did
  not share the lint's entry. Keying only the ignored directories that hold a
  file the guard reads needs the CLI to filter the fact, a protocol change.
  It is not needed: in this repository, of 242 ignored directories, 20 are not
  skipped by name, and they hold no test file the guard reads, so reading
  them changes no verdict here.
- **`build-tsc` declares `repository`.** The check needs one bit, inside a
  work tree or not, and the fact keys four states: a shallow clone, a
  complete one and a repository without a commit would keep three cache
  entries of one compile. The `check-ignore` the check runs anyway answers the
  bit.
- **`build-tsc` declares `ignoredDirectories` and answers `check-ignore` from
  it.** The listing names the ignored directories present when the run
  starts. It cannot answer for a file output (`tsconfig.tsbuildinfo` in the
  counts case) or for an outDir the build has not created yet, so the call
  stays. And the build's own outDir enters the listing once the build has
  run, so declaring it would move the build's key after its first run.
- **`validate-api` reads the working tree from `trackedFiles` and
  `ignoredDirectories`.** Its working-tree side is Git's candidate cut, which
  includes the untracked files no ignore rule excludes, and that a file
  ignored by a file rule leaves out: neither fact tells them apart.

## Consequences

- The skip guard reports a focused or skipped test in a directory git
  ignores, unless the directory is one of the names it skips: the file is in
  the lint's key, so the verdict is the key's.
- Adding a path under the project to the Git index, or removing one, moves
  the key of `build-tsc`; staging a change to a tracked file does not.
