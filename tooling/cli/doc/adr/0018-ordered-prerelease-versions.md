# ADR 0018 — Pre-release versions order by commit time

- **Status**: accepted
- **Scope**: `@putnami/cli` (`tooling/cli/internal/git`), `@putnami/typescript`
  (`typescript/extension/internal/git`), every publisher that stamps a
  pre-release version

## Context

The native selectors that pick "the newest" version compare pre-release
identifiers as strings: Go's `@latest` over `/@v/list`, and npm's highest
pre-release. A short SHA is not ordered, so a suffix made of the SHA alone makes
those selectors answer with an arbitrary commit.

## Decision

The pre-release suffix is `<commitTime>-<sha>`, with `-<dirtyHash>` appended
for a dirty tree. `commitTime` is the committer time of the bound commit in UTC,
fixed-width `yyyymmddHHMMSS`. It is one semver identifier (digits, letters,
hyphens), so npm and Go compare it as a string, and the fixed width makes string
order equal time order. The committer time, not the author time, is used
because it is what changes when a commit is rebased or amended.

The bound commit is `HEAD`, unless `PUTNAMI_SOURCE_REVISION` names the commit a
runner binds the publication to. Then `<sha>` is the first 12 characters of
that commit, and its time comes from git or, when the repository lacks the
commit, from `PUTNAMI_SOURCE_COMMIT_TIME`, which is then required.

The SHA stays the last hyphen-separated segment of a clean build: it is the
marker Distribution's cleanup uses to recognize a commit-stamped artifact.

## Consequences

- `@latest` follows the newest publication on npm and Go without a channel
  projection.
- The suffix is deterministic per commit: two publications of one commit stamp
  the same version.

## Rejected alternatives

- **The dotted form `<commitTime>.<sha>`.** A purely numeric identifier sorts
  below every alphanumeric one, which puts every new version under every old
  one.
