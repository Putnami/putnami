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
| `--evidence <path>` | Write the change evidence document to this path, resolved against the directory you run in. |
| `--upload` | Hand the change evidence document to the evidence reporter. See [Uploading the evidence](#uploading-the-evidence). |
| `--publish` | Post a commit status and one summary note on the pull or merge request. See [Publishing on the pull or merge request](#publishing-on-the-pull-or-merge-request). |

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

The commit is the one the job checked out and measured, `HEAD` or `--head`. On
a GitHub pull request that is the merge commit `refs/pull/<n>/merge`, not the
pull request's head, so the changed files are the pull request's own changes.

GitHub Actions exposes no numeric job id, so the run's job is the job key
`GITHUB_JOB`, which every leg of a matrix shares.

## The base

The base is the first of:

1. `--base`.
2. The merge request's diff base, `CI_MERGE_REQUEST_DIFF_BASE_SHA`.
3. The pull request's base commit from the event payload, else its target
   branch, `GITHUB_BASE_REF`, read on `origin`.
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
it fetches to reach the merge base (objects, the `origin` branch of the base
and the shallow boundary) and the file `--evidence` names. When the server
refuses to deepen by commit id, the history is deepened through the remote's
configured refspecs, which also updates their remote-tracking branches. The
working tree, the index and `HEAD` stay as they were. Its fetches start no
automatic Git maintenance, so nothing repacks the history after it exits.
`--no-fetch` turns the fetch off. Everything else is printed.

The fetch and the deepening need Git 2.29 or later. With an older Git, each
fails as a diagnostic.

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
most 1 000 reports, with the `observe.discovery_bounded` diagnostic. A named
glob pattern has the same bounds and the same diagnostic. A kind
with no report gets `observe.report_missing`. A named path that matches
nothing gets `observe.report_missing`, an unreadable file
`observe.report_unreadable`, and a file of another kind or none
`observe.report_unrecognized`.

## Output

The summary names the CI, the repository, the commit, the base, the changed
files (the first 50), the changed and dependent components, the tests and
coverage of each component, the coverage of the changed lines, the dependency
changes and the reports, then each diagnostic. See
[the change analyses](06-change-analyses.md). `putnami observe`
exits 0 whatever it observes, 2 on a usage error, and 1 when it cannot read its
job context or write its output. `--output=json`
prints one document with `subject`, `ci`, `producer`, `base`,
`deepenedCommits`, `changedFiles`, `reports`, `analysis` and `diagnostics`.

## The evidence document

`--evidence <path>` writes a
[`protocol/evidence`](../../../protocols/evidence/README.md) v1 document: the
subject, the CI run, the producer, the changed files with their component,
the components with their impact and edges, the tests and coverage per
component, the changed-line coverage, the dependency changes and the
diagnostics.

The document is validated before it is written, and replaces the file in one
rename. A document that does not validate, such as one without a commit or a
GitHub or GitLab job without its numeric project id, is written nowhere and
gets `observe.evidence_invalid`; a path that cannot be written gets
`observe.evidence_unwritable`. Diagnostics past the document's 500 are
summarized by `observe.too_many_diagnostics`. A run page that is not an https
URL without credentials is left out with `observe.run_url_dropped`, and the
rest of the document is written.

## Uploading the evidence

`--upload` hands the evidence document to the evidence reporter. Without
`--evidence`, the document goes to a temporary file that is removed after the
upload.

The reporter is an installed extension. `PUTNAMI_EVIDENCE_REPORTER` names it,
`@putnami/cloud` when unset. Observe runs
`putnami <name> evidence-report --file <path>`, where `<name>` is the
extension name without its scope, through the CLI that started it, with
`PUTNAMI_NO_AUTO_INSTALL=1`. The reporter's output goes to standard error. A
third party writes its own reporter as one workspace-optional subcommand
`evidence-report` that takes `--file`.

The upload never fails the job. A reporter that cannot start, exits non-zero
or runs past 2 minutes, a selector that is not an extension name, and a
document that was not written each give `observe.upload_failed`, and observe
still exits 0.

For `@putnami/cloud`, the job sets `PUTNAMI_CLOUD_TOKEN` to a workspace token
of scope `delivery.ingest`. See
[the Cloud extension](../../../cloud/extension/README.md#upload-a-change-evidence-document).

## Publishing on the pull or merge request

`--publish` posts the summary where a reviewer reads it, with a token the CI
already has. On a GitHub pull request or a GitLab merge request it:

1. Sets the commit status `putnami/observe` to success. On GitHub it goes on
   the head commit of the pull request, since the job built a merge commit.
   On GitLab it goes on the commit the pipeline ran for, `CI_COMMIT_SHA`, and
   joins that pipeline, so a merged results pipeline starts no other one. The
   status means the analysis ran, not that the tests passed. Its description
   counts the changed and dependent components, the coverage of the changed
   lines and the dependency changes, for example
   `1 changed, 2 dependent components; changed lines 80.0% covered; 1 dependency change`,
   or says that the base was not resolved. It links the CI run.
2. Writes one Markdown note on the request: the components with their tests
   and coverage, the failing tests, the least covered changed files, the
   dependency changes and the diagnostic codes. The note starts with the
   hidden line `<!-- putnami-observe:summary -->`. The next run finds the
   first note that starts with that line and updates it, so the request
   keeps one note.

| Code host | Token | API |
|---|---|---|
| GitHub | `GITHUB_TOKEN`, with `statuses: write` and `pull-requests: write` | `GITHUB_API_URL`, `https://api.github.com` when unset |
| GitLab | `PUTNAMI_GITLAB_TOKEN`: a project access token with the `api` scope and the Developer role | `CI_API_V4_URL`, else `CI_SERVER_URL` followed by `/api/v4` |

The GitLab job token cannot write notes, so GitLab needs a project access
token stored as a masked CI/CD variable. A self-managed GitLab answers on its
own URL, which GitLab CI sets in `CI_API_V4_URL`.

The token goes only to an https API URL without credentials, and observe
follows no redirect. Run `--publish` in one job of the pipeline: two jobs that
publish on the same request overwrite each other's note.

Publishing never fails the job:

| Case | Diagnostic |
|---|---|
| A generic CI, or a job that runs for no pull or merge request, such as a branch push | `observe.publish_skipped` (info) |
| The code host's token is unset | `observe.publish_token_missing` (warning) |
| A refused or failed call, no GitLab API URL, an API URL that is not https, no commit to set the status on, or a run past 1 minute | `observe.publish_failed` (warning) |

A pull request from a fork gets a read-only `GITHUB_TOKEN`, so GitHub refuses
both calls with `observe.publish_failed`. A failed status call does not stop
the note.

The note is the first one that starts with the hidden line, whoever wrote it.
A comment that starts with that line is overwritten by the summary. When the
token changes owner, for example a new GitLab project access token, the old
note may refuse the update: delete it, and the next run creates a new one.
