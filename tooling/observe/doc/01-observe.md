# What `putnami observe` reads

`putnami observe` is the default subcommand of the `observe` group. It is
interactive and workspace-optional, so it runs from the user scope in any
directory. Inside a Putnami workspace only the workspace's own extensions
answer, so the workspace must declare `@putnami/observe` to run it there.

## Flags

| Flag | Meaning |
|---|---|
| `--base <commit or branch>` | The commit the change is measured against. It overrides the CI. A generic CI needs it. |
| `--head <revision>` | The commit that was built. Default `HEAD`. |
| `--junit <paths>` | Comma-separated JUnit XML paths or glob patterns, resolved against the directory you run in. Replaces JUnit discovery. |
| `--coverage <paths>` | The same for coverage reports. Replaces coverage discovery. |
| `--no-fetch` | Read only the local history: fetch no base and deepen no shallow clone. |

A glob pattern takes `*`, `?`, `[...]` and `**`, which matches any number of
directories.

## The CI

| CI | Recognized by | Repository key | Change request |
|---|---|---|---|
| GitHub Actions | `GITHUB_ACTIONS=true` | `GITHUB_SERVER_URL` host, `GITHUB_REPOSITORY_ID` | `GITHUB_REF` `refs/pull/<n>/merge`, or the event payload |
| GitLab CI | `GITLAB_CI=true` | `CI_SERVER_HOST`, `CI_PROJECT_ID` | `CI_MERGE_REQUEST_IID` |
| Any other | neither | none | none |

The repository path (`GITHUB_REPOSITORY`, `CI_PROJECT_PATH`, or the path of
the `origin` remote) is for display only. A GitHub or GitLab job without a
numeric project id gets the `observe.project_id_missing` diagnostic.

On a GitHub pull request the commit is the pull request's head from the event
payload; the diff is measured on the commit the job checked out.

## The base

The base is the first of:

1. `--base`.
2. The merge request's diff base, `CI_MERGE_REQUEST_DIFF_BASE_SHA`.
3. The pull request's target branch, `GITHUB_BASE_REF`, read on `origin`.
4. The previous commit of a push: the event payload's `before` on GitHub,
   `CI_COMMIT_BEFORE_SHA` on GitLab, unless it is all zeros.

A base missing from the local history is fetched from `origin`. On a shallow
clone (`fetch-depth: 1` on GitHub, `GIT_DEPTH` on GitLab), the history is then
deepened by 64 commits, then 128, and so on for ten steps, until the merge base
of the base and the head is reachable. The changed files are the diff from that
merge base to the head, each `added`, `modified`, `deleted` or `renamed`.

When no base is named, none can be fetched, or the histories share no commit,
the changed files are empty and a diagnostic says why:
`observe.base_missing`, `observe.base_unresolved` or
`observe.merge_base_unreachable`.

## What it writes

In the directory you run it in, `putnami observe` writes only the Git history
it fetches to reach the merge base: objects, the `origin` branch of the base
and the shallow boundary. The working tree, the index and `HEAD` stay as they
were. `--no-fetch` turns the fetch off. Everything else is printed.

## Reports

Without `--junit` or `--coverage`, `putnami observe` walks the repository,
skipping `.git`, `node_modules` and `vendor`, and reads the start of each file
whose name can hold a report (`.xml`, `.info`, `.lcov`, `.out`, `.cov`,
`.coverprofile`, `.gocov`, and `.txt` files whose name contains `cover`). It
keeps a file by its content:

| Format | Recognized by |
|---|---|
| JUnit XML | a `testsuites` or `testsuite` root (Jest, Vitest, PHPUnit `--log-junit`, gotestsum) |
| Cobertura | a `coverage` root with `line-rate`, `lines-valid` or `branch-rate`, or a `sources` or `packages` child |
| Clover | a `coverage` root with `generated` or `clover`, or a `project` child (PHPUnit `--coverage-clover`) |
| LCOV | a first `TN:` or `SF:` record |
| Go cover profile | a first `mode: set`, `mode: count` or `mode: atomic` line |

Discovery stops after 200 000 entries or 10 000 candidate files, and keeps at
most 1 000 reports, with the `observe.discovery_bounded` diagnostic. A kind
with no report gets `observe.report_missing`. A named path that matches
nothing gets `observe.report_missing`, an unreadable file
`observe.report_unreadable`, and a file of another kind or none
`observe.report_unrecognized`.

## Output

The summary names the CI, the repository, the commit, the base, the changed
files (the first 50) and the reports, then each diagnostic. `--output=json`
prints one document with `subject`, `ci`, `producer`, `base`,
`deepenedCommits`, `changedFiles`, `reports` and `diagnostics`.
