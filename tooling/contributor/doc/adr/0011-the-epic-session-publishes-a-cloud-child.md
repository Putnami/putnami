# ADR 0011 — The epic session publishes a cloud child's proposal

- **Status**: accepted
- **Scope**: `@putnami/contributor` `epic` skill, the `build` push-only
  mode, the launch record and `putnami epic status`
- **Amends**: [ADR 0010](0010-an-epic-session-runs-issues-in-waves.md),
  sections 1 and 3, and applies
  [ADR 0008](0008-build-pushes-its-intent-before-its-code.md) to a child
  that opens no proposal

## Context

ADR 0010 launches each child in a local workspace and says the epic session
opens no proposal. A child can also run in a cloud session. That host
appends an attribution footer to each proposal body it writes, so
`change finalize` fails its readback there and cannot publish the child's
proposal. The child's run records under `.putnami/` are ignored by Git and
never reach the branch, so no other session can reuse its proof.

## Decision

### 1. `cloud` is a second launch target

A launch record's `target` is `local` or `cloud`; any other value is
refused. The policy's `epic.launch` takes the same two values. The owner
starts each session, local or cloud, from the branch, base and prompt the
epic skill names. `epic status` reports each launched child's target.

### 2. A cloud child runs build in push-only mode

`build --push-only` runs the whole loop and pushes the branch at each
checkpoint, declarations first, but opens no proposal and runs no
`change finalize`. Its end report gives the pushed head, the title, the
body and the task the proposal carries.

### 3. The epic session proves the pushed head, then publishes it

The epic session publishes the proposal of a cloud child, and only of a
cloud child. It keeps one local worktree on the child's branch, at the head
the child pushed. When the child pushes a declarations commit, the epic
session opens the draft from it, so the reviewer sees the intent before the
code, as ADR 0008 requires; the child reads the reviews once the draft
exists. At the final head, the epic session runs build's check and prove
stages there, an independent review, the final gate and the local
qualification, then build's merge stage: a dossier from that evidence, or
`--gate ci` with a workflow record it writes. A finding goes back to the child, which repairs and pushes again.
The epic session never trusts the child's records from a distance and never
commits to the child's branch.

## Consequences

- ADR 0010 section 1 holds for local children: the epic session still never
  codes and never merges, and opens a proposal only for a cloud child.
- A cloud child's review and gate run twice: once by the child, once by the
  epic session on the same head. The second run is the one the proposal
  cites.
- A cloud child that never reports its pushed head stays `in progress` in
  `epic status`; the epic session follows its session in the host.

## Alternatives rejected

- Let the cloud child finalize and strip the footer: the host appends it on
  every write, so a readback never matches.
- Push the child's `.putnami/` records with the branch: run records are
  local by design, and a dossier proved elsewhere binds another tree.
- Wait for the host to stop writing the footer: no date is known.

## Reversal

Drop the `cloud` target and the push-only mode. A cloud child then has to
finalize itself, which works once the host writes no footer.
