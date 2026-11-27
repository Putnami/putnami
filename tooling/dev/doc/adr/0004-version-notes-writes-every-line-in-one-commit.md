# ADR 0004 — `version notes` writes every line's notes in one commit

- **Status**: accepted
- **Date**: 2026-11-24
- **Scope**: `@putnami/dev` (`tooling/dev`): its manifest and
  `internal/version`
- **Extends**: [ADR 0002](0002-the-dev-extension-serves-version-get-and-tag.md)
  §1, the subcommands of the `version` group

## Context

`version tag` releases one line: one changelog, one commit, one tag. A release
of several lines goes through one reviewed pull request, and the tags go on its
merge commit. That release needs every line's notes in one commit, before any
tag.

## Decision

### 1. One subcommand that takes the lines and their versions

`putnami version notes <line>@<version>... [--dry-run] [--yes]` renders the
notes of each named line as `version tag` does and writes them into the line's
`CHANGELOG.md`. `/` names the root line. It makes one commit, and never tags or
pushes. The release owner names every version, because the lines of one release
often take a version the commits do not ask for.

### 2. It commits on the current branch, never on the default branch

The user creates the branch, as for any change. The command refuses a detached
`HEAD` and the branch that `gitread.TrunkRef` names: the branch `origin/HEAD`
points at, else `main` or `master`.

### 3. It names the other files of a release and writes none

A release also changes files that only the workspace knows, such as a dated
introduction or recorded test data. For each named line, the command reads the
commit the line's last release tag names. When that commit changed the line's
`CHANGELOG.md`, the command names the other files of the workspace it modified.
It leaves out every line's changelog and a file that no longer exists.

### 4. It refuses what a release cannot take

Besides the refusals of `version tag`, it refuses a version that is not above
the line's last release, and a version the changelog already has a section for.
The second one refuses a second run of the same command.

## Alternatives rejected

| Alternative | Why not |
|---|---|
| `version tag` with several `--scope` flags and a `--no-tag` flag | A command named for its tag that makes none, and whose one positional is a tag name |
| Tag names as arguments, such as `ts/v0.6.0` | The line is found by matching every pattern, so a typo names no line |
| A default version for a line named alone | An incomplete argument releases a version nobody named |
| A `--branch` flag that creates the branch | `git switch -c` does it, and a failure leaves one more state to undo |
| A list of release files in `putnami.workspace.json` | One more list to keep in step; the last release commit already holds it |

## Consequences

- The tags of a release prepared this way are made with git on the merge
  commit. `version tag` would write the section a second time.
- The section heading carries the day the notes were written, not the day of
  the merge. A release that waits on review corrects the date on its branch.
  A `--date` flag was not added: the tags are made by hand on the merge commit
  anyway, and the heading is one line of the reviewed diff.
- The notes list the commits the branch holds. A commit that reaches the
  default branch before the merge is under the tags but not in the notes: the
  release owner drops the notes commit, rebases and runs the command again,
  since a second run on the same notes is refused.
- A file that a release needs for the first time is not named.
- A failure of git leaves the written changelogs in the tree, as it does for
  `version tag`.
