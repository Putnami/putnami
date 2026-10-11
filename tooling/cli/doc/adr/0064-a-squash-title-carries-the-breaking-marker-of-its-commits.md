# ADR 0064 — A squash title carries the breaking marker of its commits

- **Status**: accepted
- **Scope**: `@putnami/cli` (`internal/git`, `internal/commands/versioncmd`),
  the `Pull request title` workflow of this repository

## Context

The breaking marker decides the version bump: `!` before the colon of a
commit subject, or a `BREAKING CHANGE:` footer in its body. `putnami version`
reads it from the commits on the trunk, and the API checks of `validate` read
it to allow an incompatible change.

A squash merge writes one commit and drops the commits of the branch. Its
subject is the pull request title. Its body is the pull request body only
when the host is set to keep it. A marker that sits on a branch commit alone
is in no commit of the trunk. The break then ships as a patch, and the next
`validate` on the trunk fails with no commit able to clear it.

The checks that exist cannot see this. On the branch, `validate` reads the
branch commits and passes. On the trunk, it fails after the merge, which is
too late.

## Decision

1. **`putnami version check` compares the branch with its squash message.**
   It reads the commits between `--base` and `HEAD`. It fails, with exit
   code 1, when one declares a breaking change and the squash commit message
   declares none. The message is `--title` alone, or the content of
   `--message-file`: the subject on the first line, then the body. It names
   each such commit by its 12-character ID and its subject.
2. **One reader.** The command reads the branch commits and the squash
   message with `ParseConventionalCommit` of the CLI protocol, the reader of
   the version bump and of the API checks. A title that is not a conventional
   commit declares nothing, whatever its body says, because the bump reads
   the squash commit that way.
3. **The body counts only when the caller passes it.** A footer in the pull
   request body reaches the trunk only when the host squashes with that body.
   The command cannot read that setting, so the caller states it: it writes
   the body in the message file when the squash commit keeps the body, and
   the title alone otherwise. This repository squashes with the pull request
   title and body, so its workflow writes both.
4. **Every commit of the range counts.** The check does not read the support
   catalog or the version lines. The catalog lowers the bump of a breaking
   commit outside the stable projects, but the marker still has to reach the
   trunk for the changelog and the API checks to read it.
5. **The command reads git only.** It calls no host. The pull request title
   is known to the host, so a host workflow runs the command and passes the
   title. Here that is `.github/workflows/pull-request-title.yml`, which also
   runs when the title or the body is edited. It passes the message in a
   file: the CLI reads an argument that starts with a dash as a flag, so a
   title such as `--help` given to `--title` would never reach the check.
6. **A shallow clone is refused.** It holds part of the range, so the check
   could pass without reading the commit that declares the break.
7. **No waiver.** A branch commit that declares a break the change does not
   make is reworded. The `!` marker is the only way to accept a break.

## Consequences

- A pull request whose commits declare a break cannot merge with a title
  that drops it, once the host requires the check.
- The check holds the default squash message only. A person who rewrites the
  message in the merge dialog can still drop the marker.
- A merge of the base into the branch adds no commit to the range, so a
  breaking commit the base already holds is not counted. A stale `--base`
  ref counts the commits it lacks: fetch the base before the check.
- A repository that changes its squash setting must change its workflow in
  the same step.
- The workflow runs on the `pull_request` event, so it runs the workflow file
  and the CLI of the pull request itself, as the contributor gate does. A
  pull request that edits either is seen in review, not by the check.
- Each run builds the CLI from source. A title or body edit costs one build.
