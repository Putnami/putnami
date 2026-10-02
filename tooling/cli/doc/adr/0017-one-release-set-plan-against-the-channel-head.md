# ADR 0017 — One release-set plan, measured against the channel head

- **Status**: accepted. A run whose credential provider negotiates
  `publication-v1` resolves and releases through that session instead of a
  release-set provider process
  ([ADR 0057](0057-publication-authority-stays-in-the-engine.md)).
- **Scope**: `@putnami/cli` (`tooling/cli/internal/jobs/release_set.go`,
  `tooling/cli/internal/engine`) and `@putnami/extension-sdk`
  (`tooling/extension-sdk/releaseset`)

## Context

A publication must name exactly what a channel serves. A git baseline stops
describing what the channel head published after a CI outage or a rebase, so a
selection taken from a git diff either republishes too little or refuses to
widen.

`protocols/distribution` records, on every member, the commit and the selection
fingerprint it was published from. It answers an empty channel with a null
head, and it releases a snapshot in one transaction that advances each channel
by compare-and-swap (CAS: the write succeeds only while the channel still
points at the expected head).

## Decision

1. **One plan builder, one resolve.** `buildReleaseSetPlan` works from one
   resolve that answers every listed channel, plus the `--baseline-channel`
   when one is named. The baseline head is the first listed channel's head,
   else the baseline channel's head, else none
   ([Distribution ADR 0006](../../../../protocols/distribution/doc/adr/0006-a-publication-measures-against-a-channel-it-does-not-advance.md)).
   Nothing later in the session resolves a channel again.
2. **Impact is measured against the head, not against git.** For
   `publish --impacted --channel <c>` the engine starts from every project and
   lets the plan decide. The run reports `baseline = <head id>` with source
   `release-set-head`, or `release-set-baseline-channel` when the head came
   from the baseline channel. A combined `publish,deploy` session keeps its
   git-based deploy selection and widens the publish selection to the members
   the head requires. A publish that another command expands is bounded by the
   publish jobs that command planned, and fails closed when the head requires
   more. **A gate command that shares the session with the publish keeps the
   selection the caller asked for; the coordinator narrows the publish alone.**
   `lint,test,build,validate,publish --channel <c>` plans verification over the
   selection the flags resolved (the git-impacted set under `--impacted`, the
   ordinary default under `--all`) and plans publication over the members the
   head requires. The run plans the union. The session reports both sets:
   `selection.projects` is the union and `selection.releaseSetProjects` the
   publication half. The mode, baseline and tier it reports are the gate's,
   because they describe how the verification set was chosen.
3. **Selection.** A member is selected when `--all` is given, when there is no
   baseline head, when the head does not carry the member, or when the member's
   selection fingerprint
   ([ADR 0021](0021-publish-version-and-deploy-follow-distribution-v2.md) §2)
   differs from the head's record. Every member that depends on a selected
   member is selected too, so every dependency record names a version in the
   set. An unselected member inherits its head artifact record exactly.
   Membership follows the workspace: a plan may add or drop coordinates
   relative to the head. A plan that selects nothing is an ordinary plan.
   `ReleaseSetImpacted` and `ReleaseSetAll` name the selection rule only: there
   is no bootstrap mode and no sparse or full mode.
4. **Provenance.** Every selected member records the source revision: the
   commit `PUTNAMI_SOURCE_REVISION` binds when a runner sets it, else the full
   `HEAD` commit. The selection never reads the revision or the git history
   between two publications.
5. **One release call.** The finalizer, or the same-session barrier, builds the
   next snapshot from the verified publication events and calls `release` once.
   Each listed channel carries its own resolved head as expectation; an empty
   channel asserts that it has no head. `released` and `already-current`
   succeed. `conflict` fails, names the expected and observed head of every
   channel that moved, and advances no channel. A plan that selects nothing
   releases the head set unchanged and is answered `already-current`. The
   finalizer withholds the release when any job failed or was canceled, and
   refuses it, naming the jobs, when a selected member's publish job was
   skipped. A dry run releases nothing. The `data.releaseSet` outcome names the
   new snapshot's ref, so a same-session deploy consumes exactly what was
   published.

## Consequences

- A mixed verification and publication run keeps the providers of the
  requested verification commands, including SDD validation, and the projects
  those commands selected. Unrelated publish jobs stay excluded. A publish node
  of a project that owns no selected member is dropped together with the
  package nodes that only fed it. When no member changes, verification still
  runs over the gate's selection. A publish-only session, and a gate that
  selected nothing, plan no job; the empty-plan refusal does not apply to them,
  and the finalizer still confirms the head from the local process.
- `publish --impacted` and `publish --all` work on an empty channel and on a
  channel with a head.
- A run that follows a CI gap publishes exactly the members whose fingerprint
  changed since the head, whatever the number of skipped commits.
- The `cloud-release-set` provider implements the protocol version 2 `resolve`
  and `release` operations. The CLI sends version 2 requests only.

## Rejected alternatives

- **Select from a git diff.** A diff stops matching the head after a CI gap or
  a rebase.
- **Advance a channel without an expectation.** Concurrent publishers would
  overwrite each other and report success from stale plans.
