# ADR 0007 — A publication may reuse a member another channel published with the same fingerprint

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/distribution` (`protocols/distribution`),
  `putnami-extension-sdk` (`tooling/extension-sdk/releaseset`), `@putnami/cli`,
  `go.putnami.dev/protocol/job`, `go.putnami.dev/protocol/cli`
- **Amends**: [ADR 0006](0006-a-publication-measures-against-a-channel-it-does-not-advance.md),
  decision 1 and the rejected alternative "both heads, matched per member by
  fingerprint"

## Context

A pull request publishes the members it changed into `pr-N`, measured against
`canary` ([ADR 0006](0006-a-publication-measures-against-a-channel-it-does-not-advance.md)).
When it merges, `main` measures the same tree against `canary`, selects the same
members, and builds and uploads each of them again. The artifacts it uploads
come from the same packaging recipe: the selection fingerprint, the package
task's execution key without the version
([ADR 0001](0001-immutable-release-sets-and-channel-cas.md)), is the one the
pull request recorded.

ADR 0006 rejected matching both heads per member by fingerprint. That
alternative used the second head to decide which members are selected, so the
SDK would have accepted two inheritance sources, and a reader could no longer
name the single head a set derives from.

## Decision

1. **A publication may name one reuse channel it reads and never advances**
   (`publish --reuse-channel <c>`). It differs from every advanced channel and
   from the baseline channel. The one resolve names the advanced channels, then
   the baseline, then the reuse channel, and `MaxChannelsPerRelease` (16)
   bounds the whole list. The reuse channel is never a `ChannelRequest`, never
   the baseline, and never appears in `ReleaseSetPublishOutcome`.
2. **Selection is unchanged.** The baseline head is chosen and every member is
   selected or inherited exactly as ADR 0006 states. The reuse head never
   selects a member, never drops one, and is never inherited from.
3. **A selected member is reused** when the reuse head carries it with the
   selection fingerprint this run computed, and:
   1. when the baseline head carries the member, the reused version sorts
      strictly above the baseline version under semver precedence, so a channel
      never moves a member backwards;
   2. the reused version and this run's candidate version have the same base
      version, and differ only by the commit-derived ordered suffix. Both carry
      that suffix or neither does.

   A reused member whose recorded dependencies are not the final versions of
   those dependencies is built after all, and its dependents are checked again,
   until no reused record changes. A publication with `--all`, or a tagged one,
   reuses nothing.
4. **A reused member is the reuse head's record, rebound to this commit.**
   Version, digest, dependencies, fingerprint, platforms, project and kind are
   the reuse head's, with attribution stamped like an inherited member's
   ([ADR 0005](0005-member-attribution-project-and-kind.md)).
   `sourceRevision` and `sourceTree` are this publication's. A reused
   member's revision names the commit whose tree the release vouches for by
   fingerprint, not the commit its build ran on. A squash merge drops the pull
   request's commit from `main`'s history, and a head whose revisions `main`
   does not hold is not an ancestor of the next `main` commit.
5. **A reused member is not built.** It is unselected, so the plan gives it no
   package step and no publish step, and nothing is uploaded for it.
6. **The plan carries it**, and only when something was reused.
   `releaseset.Plan` names `reuseChannel`, the sorted `reused` member list, and
   `sourceRevision`, the commit the plan publishes; the reuse head rides in
   `heads`. The SDK replays the resolve selector with the reuse channel
   appended. It accepts a reused member only when it equals the reuse head's
   record, apart from `sourceRevision` and `sourceTree`, and records the
   plan's `sourceRevision`. A plan that reuses nothing has the same bytes as
   one that names no reuse channel.
7. **No wire change to `resolve` or `release`.** The reuse channel is one more
   requested name in `ResolveRequest.channels`. A `release` carries the final
   set like any other.

## Why this does not reopen the rejected alternative

- One head still decides the set. The baseline head alone selects members and
  supplies every inherited record. The reuse head only supplies the artifact
  for a member the baseline already selected for republication.
- The SDK still accepts one inheritance source. An unselected member that is
  not listed in `reused` must equal the baseline record. A reused member must
  be listed by name and equal the reuse head's record.
- A reader can still name the head a set derives from. `selection.baseline`
  names the baseline head, and `selection.reuse` names the reuse channel, its
  head and every reused member.

## Trust

A reused artifact is trusted as far as an inherited one. Neither is rebuilt or
checked byte for byte by the publication: an inherited member relies on the
writer of the baseline channel, and a reused member relies on the writer of the
reuse channel, both under the same namespace and the same provider. A
repository names as reuse channel only a channel whose writers it trusts as
much as the writers of the channel it advances. The provider resolves
visibility for a reused member under the advanced channel's level, like every
other member of the set.

## Rejected alternatives

- **Keep the reuse head's `sourceRevision`.** The next `main` publication
  would find a revision its history does not hold after a squash merge, and
  refuse its ancestry.
- **Reuse an older version than the baseline's.** The channel would serve an
  older build of a member it already served a newer build of.
- **Reuse a member whose dependency is rebuilt.** Its record would name a
  dependency version the set does not carry, and the set would not be closed.
- **Reuse on `--all` and tagged publications.** Both republish their members
  whatever a head records; a tag publishes the tag's version, which no other
  channel carries.
- **A capability gate for older extensions.** As in ADR 0006, the CLI cannot
  see what an installed extension was compiled against. An older SDK refuses a
  plan that reuses a member, because it did not request the reuse head, and
  meets one only when something was reused (decision 6).

## Consequences

- The SDK and the CLI ship together; an extension on an older SDK fails a
  publication that reuses a member.
- A reused member's version keeps the reuse publication's ordered suffix, so
  its commit part names the pull request's commit.
- A publication-v1 broker accepts a reused member as a member carried by a
  resolved head, rebound to the plan's revision.
- The publication sends no image evidence for a reused OCI member: evidence
  covers the members it built.
