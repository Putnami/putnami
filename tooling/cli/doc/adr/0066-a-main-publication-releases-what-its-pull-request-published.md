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

1. **`publish --reuse-channel <c>`** sits beside `--baseline-channel`.
   `BuildReleaseSetOptions` refuses, as a usage error, a name that is not
   portable, that one of the `--channel` names advances, or that is the
   `--baseline-channel`, and a list of advanced, baseline and reuse channels
   longer than `MaxChannelsPerRelease`. `ValidateReleaseSetSelection` refuses
   it without a channel to publish, with `--all`, on a tagged publish, and
   without `--impacted`.
2. **One resolve.** `releaseSetResolveChannels` names the advanced channels,
   then the baseline, then the reuse channel (ADR 0017 §1 as amended).
3. **The reuse pass runs after selection.** The baseline head and the selected
   members are decided exactly as ADR 0017 §3 states, dependency closure
   included, and every selected member gets its candidate version. Then
   `reuseSelectedMembers` takes, for each selected member, the reuse head's
   record when it has the fingerprint this run computed, an artifact digest, a
   version strictly above the baseline head's record of the member when there
   is one, and the candidate's base version with only the commit-derived
   ordered suffix changed. A last pass, repeated until nothing changes, builds
   again every reused member whose recorded dependencies differ from the final
   versions. `--all` and tagged publications reuse nothing.
4. **A reused member is unselected.** It is the reuse head's record, rebound to
   this run's source revision and source tree, attributed like an inherited
   member. Because it is unselected, `ScopePlanning`, `AttachPlan` and the
   finalizer treat it like an inherited member: no package step, no publish
   step, and no published-member event is expected. A plan whose selected
   members are all reused selects nothing, and the finalizer still releases
   the new set (ADR 0017 §3 and §4 as amended).
5. **The plan names the reuse only when it happened.** `recordReleaseReuse`
   sets `reuseChannel`, `reused`, `sourceRevision` and the reuse head in
   `heads` when at least one member was reused. A publication that names a
   reuse channel and reuses nothing hands the extensions the same plan bytes
   as one that names none.
6. **The session reports it.** `selection.reuse` names the reuse channel, the
   id of its resolved head and every reused member with its version, in the
   job-context selection and in the session file. It is absent when nothing
   was reused. `releaseSetProjects` does not list a project whose members were
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
  flag.

## Rejected alternatives

- **Reuse before the dependency closure.** A reused member would stop selecting
  its dependents, and selection would depend on a head other than the
  baseline, which Distribution ADR 0006 rejects.
- **Plan a publish job that skips the upload.** The job would still package
  the member and the coordinator would have to accept an event for an
  artifact it did not build.
