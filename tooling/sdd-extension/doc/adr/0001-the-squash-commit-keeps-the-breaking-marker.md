# ADR 0001 — The squash commit keeps the breaking marker of its commits

- **Status**: accepted
- **Scope**: `@putnami/sdd` (`breaking-marker-validate`, a step of
  `validate-workspace`), the Contributor CI workflow of this repository, and
  every runner that runs the gate on a pull request

## Context

The breaking marker decides the version bump: `!` before the colon of a
commit subject, or a `BREAKING CHANGE:` footer in its body. `putnami version`
reads it from the commits on the base branch, and the API checks of `validate`
read it to allow an incompatible change.

A squash merge writes one commit and drops the commits of the branch. Its
subject is the pull request title. Its body is the pull request body only when
the repository squashes with it. A marker that only a branch commit carries is
in no commit of the base branch. The break then ships as a minor or a patch,
and the next `validate` on the base branch fails with no commit able to clear
it.

The checks that exist cannot see this. On the branch, `validate` reads the
branch commits and passes. On the base branch, it fails after the merge, which
is too late.

## Decision

1. **A step of `validate-workspace` compares the squash message with the
   commits of the change.** Every gate that runs `validate` runs it, on any
   runner, so no workflow and no command of its own has to be maintained. It
   fails when a commit declares a breaking change and the squash message does
   not, and it names each such commit by its 12-character ID and its subject.
2. **The squash message comes from `PUTNAMI_PROPOSAL_MESSAGE`.** It holds the
   subject, a blank line, and the body when the squash keeps one. The step
   splits it as Git splits a commit message: the subject is the first
   paragraph, so a footer with no blank line above it is part of the subject
   and declares nothing. An unset or blank variable passes with a note: a run
   outside a pull request has no squash message to compare.
3. **The runner owns the variable.** A runner that runs the gate on a pull
   request sets it in the environment of the `putnami` process: the title, a
   blank line and the body when the repository squashes with the body, the
   title alone otherwise. It runs `validate` again when the title or the body
   is edited. Contributor CI sets it on `pull_request` events. The hosted
   Putnami CI runner does not set it yet.
4. **The range is the run's impacted baseline to `HEAD`.** The step reads the
   baseline that `--impacted` resolved from the job context. A set message in
   a run that is not `--impacted` fails, because there is no range to compare
   it with, and a pass would approve a range the step never read. A merge of
   the base into the branch brings no commit of the base into the range.
5. **One reader.** The step reads every message with
   `ParseConventionalCommit` of the CLI protocol, the reader of the version
   bump and of the API checks. A title that is not a conventional commit
   declares nothing, whatever its body says, because the bump reads the squash
   commit that way.
6. **The body counts only because the runner writes it.** A footer in the pull
   request body reaches the base branch only when the repository squashes with
   that body. The step cannot read that setting, so it trusts the runner to
   write the body only then.
7. **Every commit of the range counts.** The step does not read the support
   catalog or the version lines. The catalog lowers the bump of a breaking
   commit outside the stable projects, but the marker still has to reach the
   base branch for the changelog and the API checks to read it.
8. **The step is never served from cache.** Its verdict reads an environment
   variable and Git history, and the candidate cut holds neither. A key would
   need the commits of the range, and
   [ADR 0059 of the CLI](../../../cli/doc/adr/0059-a-cache-key-describes-the-tree-not-the-commit.md)
   keeps commits out of every key. The task declares the variable as an `env`
   input to state the contract, and declares its cache disabled.
9. **A shallow clone is refused.** It holds part of the range, so the step
   could pass without reading the commit that declares the break.
10. **No waiver.** A branch commit that declares a break the change does not
    make is reworded. The `!` marker is the only way to accept a break.

## Consequences

- A pull request whose commits declare a break fails the gate when its title
  drops the marker, on every runner that sets the variable.
- Until the hosted Putnami CI runner sets the variable, only Contributor CI
  checks the marker. Contributor CI does not run when the title or the body is
  edited, so an edit after the last push is checked at the next push.
- The step checks the default squash message only. A person who rewrites the
  message in the merge dialog can still drop the marker.
- The step does not run when `--impacted` selects no project: the run then
  plans no job at all.
- The step reads every commit of the range. A branch stacked on a parent that
  has since been squash-merged, or a breaking commit reverted later on the
  branch, fails although the squash commit breaks nothing. The remedy is to
  rebase onto the base and drop those commits, not a waiver.
- The variable travels in the process environment only. A run with
  `--where remote` does not carry it, so the step passes there with the note
  "no proposal message to compare". The runner that sets the variable runs
  the gate locally.
- The variable holds the title and the body. A body near the operating
  system's limit for one environment variable (128 KiB on Linux) cannot be
  passed, and the runner fails before the gate starts.
- A stale baseline counts the commits it lacks. The runner fetches the base
  branch before the gate, as Contributor CI does with its full clone.
- A repository that changes its squash setting changes what its runner writes
  in the variable in the same step.
- The gate runs the CLI and the extension of the pull request itself. A pull
  request that edits the step is seen in review, not by the step.
