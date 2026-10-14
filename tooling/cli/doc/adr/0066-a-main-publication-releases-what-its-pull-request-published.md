# ADR 0066 — A main publication releases what its pull request published

- **Status**: accepted
- **Scope**: `@putnami/cli` (`tooling/cli/internal/jobs/release_set.go`,
  `tooling/cli/internal/engine`, `tooling/cli/internal/cli`,
  `tooling/cli/internal/machine`)
- **Amends**: [ADR 0017](0017-one-release-set-plan-against-the-channel-head.md),
  decisions 1, 3 and 4

## Context

A pull request publishes the members it changed into `pr-N`, measured against
`canary`. After the merge, the `main` publication of the same tree selects the
same members against `canary` and builds and uploads each of them again, though
`pr-N` already carries an artifact with the same selection fingerprint.
[Distribution ADR 0007](../../../../protocols/distribution/doc/adr/0007-a-publication-may-reuse-a-member-another-channel-published-with-the-same-fingerprint.md)
lets a publication release that artifact instead. This ADR records how the CLI
plans it.

## Decision

1. **`publish --reuse-channel <c> --reuse-source-revision <commit>`** sits
   beside `--baseline-channel`. `BuildReleaseSetOptions` refuses, as a usage
   error, a name that is not portable, that one of the `--channel` names
   advances, or that is the `--baseline-channel`, and a list of advanced,
   baseline and reuse channels longer than `MaxChannelsPerRelease`.
   `ParseReleaseSetReuseSourceRevision` refuses a reuse channel without its
   source revision, a source revision without a reuse channel, and a value
   that is not a full 40-hex commit once trimmed and lowercased, the rule
   `PUTNAMI_SOURCE_REVISION` follows. The runner states the revision: the
   pull request's reviewed final head. A pull-request channel also carries
   records inherited from earlier pushes, and an earlier push may have
   published a tampered artifact whose fingerprint equals the reverted tree's,
   so a reuse channel is never trusted without the commit whose build it
   vouches for. On a tagged publish, after these checks, it moves the
   channel to `IgnoredReuseChannel`, clears the revision, and the engine
   prints one notice line
   (`ReleaseSetReuseNotice`): the tag republishes its version line at the
   tag's version, which no other channel carries, and a runner may pass the
   flag to every publication of a branch that also gets tagged.
   `ValidateReleaseSetSelection` answers the tagged case first and refuses
   nothing there; otherwise it refuses the flag without a channel to publish,
   with `--all`, and without `--impacted`.
2. **One resolve.** `releaseSetResolveChannels` names the advanced channels,
   then the baseline, then the reuse channel (ADR 0017 §1 as amended).
3. **The reuse pass runs after selection.** The baseline head and the selected
   members are decided exactly as ADR 0017 §3 states, dependency closure
   included, and every selected member gets its candidate version. Then
   `reuseSelectedMembers` takes, for each selected member, the reuse head's
   record when it has the fingerprint this run computed, exactly the reuse
   source revision as its `sourceRevision`, an artifact digest, a
   version strictly above every record of the member in the baseline head and
   in the head of each advanced channel (`releaseVersionFloors`), and the
   candidate's base version with only the commit-derived ordered suffix
   changed. A last pass, repeated until nothing changes, builds
   again every reused member whose recorded dependencies differ from the final
   versions. `--all` and tagged publications reuse nothing, and so does a
   run without a reuse source revision. A record the reuse head inherited
   from an earlier push of the pull request is built instead.
4. **A reused member is unselected.** It is the reuse head's record, rebound to
   this run's source revision and source tree, attributed like an inherited
   member. The tree is the one a member this run builds records
   (`provenance.tree`), even when the run builds none, and the commit drops it
   like a built member's when the checkout moved
   (`dropRepublishedSourceTree`). A reader names a head by one tree only when
   every member carrying its revision records the same one, so a reused member
   without it would leave the head without a tree. Because it is unselected,
   `ScopePlanning`, `AttachPlan` and the finalizer treat it like an inherited
   member: no package step, no publish step, and no published-member event is
   expected. A plan whose selected
   members are all reused selects nothing, and the finalizer still releases
   the new set (ADR 0017 §3 and §4 as amended).
5. **The plan names the reuse only when it happened.** `recordReleaseReuse`
   sets `reuseChannel`, `reuseSourceRevision`, `reused`, `sourceRevision` and
   the reuse head in `heads` when at least one member was reused. The SDK
   refuses a plan whose reused member's record on the reuse head has another
   revision than `reuseSourceRevision`, so an extension verifies the
   restriction instead of trusting the CLI that applied it. A publication that names a
   reuse channel and reuses nothing hands the extensions the same plan bytes
   as one that names none.
6. **The session reports it.** `selection.reuse` names the reuse channel, the
   id of its resolved head, the reuse source revision as `sourceRevision`, and
   every reused member with its version, in the
   job-context selection and in the session file. It states the plan, not the
   release outcome: a dry run, or a release that fails, records the same
   block. It is absent when nothing was reused. `releaseSetProjects` does not list a project whose members were
   all reused, because the session ran none of its publish steps.

## Consequences

- A `main` run after a merged pull request uploads nothing for the members the
  pull request published, and its head stays an ancestor of the next `main`
  commit after a squash merge, because each reused member names `main`'s
  commit.
- An extension built on an older SDK refuses a plan that reuses a member.
- The plan is not a cache-key input, so naming a reuse channel moves no task
  key.
- `putnami.ci.json` does not declare a reuse channel; the runner passes the
  two flags, and the revision is the source revision the run of the pull
  request's final head recorded, as the runner knows it, not a value the CLI
  derives from the reuse head. A runner that builds a merge commit for a pull
  request passes that merge commit; any other value matches no record and
  reuses nothing. It may pass them
  on a branch that gets tagged, because a tagged publish ignores them.
- A pull request whose final push published nothing for a member, because the
  member's fingerprint did not change since an earlier push, leaves that
  member's record at the earlier commit, and the `main` publication builds it
  again. Reuse trades that rebuild for the guarantee that only artifacts the
  run of the reviewed final head built are reused.
- A runner passes the two flags only once the CLI and every extension it
  pins know the reuse fields (Distribution ADR 0007, Consequences).

## Rejected alternatives

- **Reuse before the dependency closure.** A reused member would stop selecting
  its dependents, and selection would depend on a head other than the
  baseline, which Distribution ADR 0006 rejects.
- **Plan a publish job that skips the upload.** The job would still package
  the member and the coordinator would have to accept an event for an
  artifact it did not build.
- **Trust every record the reuse head carries.** The head of `pr-N` keeps a
  member an earlier push published when a later push does not rebuild it. A
  push that tampered with the member and a push that reverted the tampering
  leave the earlier artifact behind with the fingerprint of the reverted
  tree, and `main` would release it as its own build.
- **Read the revision from the reuse head.** The head's own records cannot
  say which push was reviewed last; only the runner knows the final head of
  the pull request, so it states it.
