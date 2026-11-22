# ADR 0002 — A fork's evidence is relayed by the base repository

- **Status**: accepted
- **Date**: 2026-11-22
- **Scope**: `@putnami/observe` (`putnami observe --from-evidence`, `action/`),
  features `tooling/observe-fork` and `tooling/observe-action`

## Context

GitHub runs the workflow of a pull request from a fork with no ID token and a
read-only `GITHUB_TOKEN`, so `--upload` and `--publish` both fail there. Most
outside contributions to an open-source repository come from forks. A
`pull_request_target` job holds the base repository's credentials, but it
checks out and runs the fork's code with them, which hands those credentials
to the fork.

## Decision

The fork's run observes the change as any run does and keeps its evidence
document as an artifact. A second workflow of the base repository, started by
`workflow_run` when the first completes, runs no code of the fork: it
downloads the artifact and runs `putnami observe --from-evidence <file>
--upload --publish` with its own ID token and `GITHUB_TOKEN`.

The document is untrusted input. `--from-evidence`:

- relays only in a job that a `workflow_run` event started for a
  `pull_request` run, reads the pull request and the commits with the job's
  `GITHUB_TOKEN`, and takes the
  repository, its numeric id and its host from the job, and the run, its
  page, its head commit and its head branch from the event payload;
- finds the pull request in the payload, or, for a fork, whose payload lists
  none, asks the GitHub API for the open pull request of the job's repository
  whose head is the run's head owner, branch and commit;
- refuses a document whose subject names another repository or pull request
  than the job's, or that does not validate, and bounds it at 64 MiB, the
  bound the `@putnami/cloud` reporter reads;
- binds the document to the run: it accepts only the run's head commit,
  with a base the GitHub API answers as an ancestor of it, or the test merge
  commit GitHub keeps for the pull request (`merge_commit_sha`), whose
  parents are the document's base and the run's head; it relays no branch
  name of a fork, and a fork's run whose pull request is not found relays
  nothing, so no record of the base repository carries a commit or a branch
  the fork chose;
- keeps the document's analyses as the fork's claim, and quotes every text of
  them in the note as a code span; the job log prints counts and observe's own
  diagnostics only, so no line of the document becomes a workflow command.

## Rejected alternatives

- `pull_request_target` with a checkout of the fork's head: the fork's code
  runs with the base repository's credentials.
- Trusting the pull request number the document names: a fork could post its
  note on another pull request.
- Downloading the artifact in `putnami observe`: the CLI would hold
  `actions: read` and unpack an archive the fork wrote; `actions/download-artifact`
  already does both, and the action composes it.

## Consequences

- A repository that takes forks commits two workflows: the pull request one,
  unchanged except that it keeps the artifact, and the `workflow_run` one.
- The report and the note of a fork show what the fork's run claims. The
  commit status names the base repository's check, set by its own token, on
  the fork's head commit.
