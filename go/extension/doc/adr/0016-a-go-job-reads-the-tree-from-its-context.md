# ADR 0016 — A Go job reads the tree from its context, or from the files its key holds

- **Status**: accepted
- **Scope**: `@putnami/go` (`go/extension`): the skip guard of the
  golangci-lint tasks, `validate-api`, and the package version
- **Applies**: job protocol
  [ADR 0002](../../../../protocols/job/doc/adr/0002-a-job-reads-the-tree-facts-its-key-holds.md);
  amends [ADR 0010](0010-the-api-check-compares-source-not-types.md)

## Context

The Go jobs asked Git three questions the engine can answer once per run: the
skip guard listed the directories Git ignores (`ls-files --others --ignored
--directory`, one per lint job), `validate-api` read the repository state, the
line's last release and the files its tag holds (`rev-parse`, `for-each-ref`,
`ls-tree`, `cat-file`, `log`), and the package task read a commit and a dirty
state when its context carried no full version (`rev-parse`, `status`,
`diff`). On the Go case of the orchestration counts, the jobs made 2 Git
calls, both `ls-files`: the skip guard's and the docs lint's.

## Decision

1. **The skip guard reads what the lint key holds.** The golangci-lint tasks
   key their sources with ordinary patterns (`**/*.go`, `!**/.gen/**`), whose
   file walk skips `node_modules`, `.git`, `.putnami`, `out`, `dist` and
   `vendor` and follows no ignore rule. The guard skips those names, dot and
   underscore directories, `testdata` and nested modules and projects, and no
   longer skips the directories Git ignores: it reads a subset of the key's
   files, its verdict depends on nothing else, and it starts no Git. The tasks
   declare no tree fact.
2. **`validate-api` declares `releaseBaselineFiles`.** The release baseline
   and the regular files its tag holds at the project come from the job
   context; the check reads Git only when the context carries no release
   baseline. Both readings hold regular files only, and read the tag's files
   at the commit the line's last release resolved to.
3. **The package version comes from the context the orchestrator wrote.** A
   job context of version 2 without a version says the orchestrator read none,
   and gives the suffix `nogit`; only a direct invocation asks Git.

Measured after the change on the Go case of the orchestration counts: the jobs
make 1 Git call, the docs lint's `ls-files`, which the candidate cut of job
protocol ADR 0003 covers; the engine makes the same 18 calls.

## Consequences

- A directory Git ignores that holds `_test.go` files is scanned by the skip
  guard, as `go test ./...` runs those tests. A generated tree under a dot
  directory, `out` or `dist` is not.
- A build, which fills `bin/`, `dist/` or `.gen/`, moves no lint key: a fresh
  clone and a built checkout share the lint's cached verdict.
- A released command surface the tag holds as a symbolic link gives a
  not-compared warning instead of an error, and a `putnami.json` the tag holds
  as a link declares no surface, on the Git reading as on the context's.

## Rejected alternatives

- **The lint tasks declare `ignoredDirectories`, and the guard skips the
  directories the context lists.** `ls-files --ignored --directory` lists
  nothing on a fresh clone and `bin/`, `dist/` and `.gen/` after a build, so
  the lint key would differ between a fresh and a built checkout of one tree,
  and a cached verdict would be lost to every build.
