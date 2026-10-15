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
   (`publish --reuse-channel <c> --reuse-source-revision <commit>`). It
   differs from every advanced channel and from the baseline channel. The
   reuse source revision is the commit whose build the reuse channel may
   offer: the pull request's reviewed final head, which the runner states.
   The two flags go together, and the revision is a full 40-hex commit under
   the rule every other revision input follows. The one resolve names the advanced channels, then
   the baseline, then the reuse channel, and `MaxChannelsPerRelease` (16)
   bounds the whole list. The reuse channel is never a `ChannelRequest`, never
   the baseline, and never appears in `ReleaseSetPublishOutcome`.
2. **Selection is unchanged.** The baseline head is chosen and every member is
   selected or inherited exactly as ADR 0006 states. The reuse head never
   selects a member, never drops one, and is never inherited from.
3. **A selected member is reused** when the reuse head carries it with the
   selection fingerprint this run computed and with exactly the reuse source
   revision as its `sourceRevision`, and:
   1. the reused version sorts strictly above, under semver precedence, the
      member's record in the baseline head and in the head of every channel
      the publication advances, wherever one carries the member, so no channel
      moves a member backwards;
   2. the reused version and this run's candidate version have the same base
      version, and differ only by the commit-derived ordered suffix. Both carry
      that suffix or neither does.

   A record the reuse head carries from another commit, such as one an
   earlier push of the pull request published and a later push did not
   rebuild, is built instead. A reused member whose recorded dependencies are
   not the final versions of those dependencies is built after all, and its dependents are checked again,
   until no reused record changes. A publication with `--all` refuses a reuse
   channel. A tagged publication ignores it with one notice line and reuses
   nothing: the tag republishes its line at the tag's version, which no other
   channel carries.
4. **A reused member is the reuse head's record, rebound to this commit.**
   Version, digest, dependencies, fingerprint, platforms, project and kind are
   the reuse head's, with attribution stamped like an inherited member's
   ([ADR 0005](0005-member-attribution-project-and-kind.md)).
   `sourceRevision` and `sourceTree` are this publication's. The tree is the
   value a member this publication builds records, under the same rules
   ([ADR 0005](0005-member-attribution-project-and-kind.md): only on an
   opted-in clean checkout, dropped at commit when the checkout moved), even
   when the publication builds no member. A reused member's revision and tree
   name the commit and the content the release vouches for by fingerprint,
   not the checkout its build ran on. A squash merge drops the pull request's
   commit from `main`'s history, and a head whose revisions `main` does not
   hold is not an ancestor of the next `main` commit. A reader that names a
   head by one tree, such as deploy provenance, can name one only when every
   member carrying the head's revision records the same tree, so a reused
   member without the run's tree would leave every head that reuses anything
   without one.
5. **A reused member is not built.** It is unselected, so the plan gives it no
   package step and no publish step, and nothing is uploaded for it.
6. **The plan carries it**, and only when something was reused.
   `releaseset.Plan` names `reuseChannel`, `reuseSourceRevision`, the sorted
   `reused` member list, and `sourceRevision`, the commit the plan publishes;
   the reuse head rides in `heads`. The SDK replays the resolve selector with
   the reuse channel appended. It accepts a reused member only when it equals
   the reuse head's record apart from `sourceRevision` and `sourceTree`, when
   that record's `sourceRevision` is the plan's `reuseSourceRevision`, and
   when it records the plan's `sourceRevision`. In a plan that reuses a member, every selected and reused
   member records one `sourceTree`, possibly none. A plan that reuses nothing
   has the same bytes as one that names no reuse channel.
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
  head, the reuse source revision and every reused member.

## Trust

Only artifacts built by the run of the reviewed final head are reused. A
pull-request channel carries what every push of the pull request published: a
push that does not rebuild a member keeps the record an earlier push wrote.
Push 1 can tamper with a member in a way its fingerprint does not see, and
push 2 can revert the tampering; `pr-N` then still carries push 1's artifact
with the fingerprint the merged tree computes. A record is reusable only when
its `sourceRevision` is the reuse source revision, the final head the runner
states, so push 1's record is built again and the merge releases nothing a
reviewer did not see built. A record stamped with the final head was released
by that run, built by it or reused by it under these same rules. The CLI
applies the restriction, and the SDK verifies it on the plan, so an extension
does not have to trust the CLI that applied it. The runner is trusted to name
the reviewed final head; the CLI does not derive it from the reuse head,
whose records cannot say which push a reviewer approved.

A reused artifact is trusted as far as an inherited one. Neither is rebuilt or
checked byte for byte by the publication: an inherited member relies on the
writer of the baseline channel, and a reused member relies on the writer of the
reuse channel, both under the same namespace and the same provider. A
repository names as reuse channel only a channel whose writers it trusts as
much as the writers of the channel it advances. The provider resolves
visibility for a reused member under the advanced channel's level, like every
other member of the set.

A publication-v1 broker cannot tell the reuse head from the other heads a
session resolved: the plan tuple it opens carries neither the reuse channel nor
the reused members, and no fingerprint for an unselected member. It accepts an
unplanned member rebound to the plan's revision only when the head of a channel
the plan does not advance carries the same artifact. The member names no
`sourceTree`, or the one tree that every planned and rebound member bound to
the plan's revision records; an inherited member is another publication's
record and does not count. When those members record two trees, an empty one
included, no rebound member may name a tree. A head the plan advances never
lends its artifact a new revision, so an artifact cannot be relabelled as built
from this commit on the channel it already sits on. The broker checks that the
tree is consistent, not that it is the checkout's: the plan tuple carries no
tree, and it does not check the reuse source revision either: the plan tuple
carries none, and the CLI and the SDK enforce it. Binding the reuse channel,
its source revision and the reused members, with their fingerprints, into the
plan tuple would let the broker and the provider check reuse against the
frozen plan. That changes the publication plan protocol, and
is deferred.

## Rejected alternatives

- **Keep the reuse head's `sourceRevision`.** The next `main` publication
  would find a revision its history does not hold after a squash merge, and
  refuse its ancestry.
- **Trust every record the reuse head carries.** A record an earlier push
  published survives a later push that does not rebuild the member, and its
  fingerprint can match a tree that push reverted (Trust).
- **Read the reuse source revision from the reuse head.** The head carries
  the records of several pushes, and none of them says which push a reviewer
  approved. Only the runner knows the final head.
- **Reuse an older version than the baseline's.** The channel would serve an
  older build of a member it already served a newer build of.
- **Reuse a member whose dependency is rebuilt.** Its record would name a
  dependency version the set does not carry, and the set would not be closed.
- **Reuse on `--all` and tagged publications.** Both republish their members
  whatever a head records; a tag publishes the tag's version, which no other
  channel carries. `--all` refuses the flag. A tagged publication ignores it
  instead, because a runner passes the flag on a branch that also gets tagged,
  and refusing it would fail that tag's release.
- **Keep the reuse head's `sourceTree`, or record none.** The reuse head's tree
  would pair this commit's revision with another commit's content. No tree
  would leave every head that reuses a member without a tree, because the
  members carrying its revision would no longer agree on one.
- **A capability gate for older extensions.** As in ADR 0006, the CLI cannot
  see what an installed extension was compiled against. An older SDK refuses a
  plan that reuses a member, because it did not request the reuse head, and
  meets one only when something was reused (decision 6).

## Consequences

- **Rollout order.** The SDK, the CLI and the publication-v1 broker ship
  together. An extension built on an older SDK refuses a plan that reuses a
  member, and a broker built before this ADR refuses the release, so the
  publication fails. A runner passes `--reuse-channel` and
  `--reuse-source-revision` only once its pinned
  CLI, and every extension it pins that runs a package or publish step for a
  member, know the reuse fields.
- **Provider obligations outside this repository.** Each holds, proven where
  it is implemented, before a runner passes the flag:
  1. the loopback broker an image runs for a publication without
     publication-v1 accepts an unplanned member rebound to the run's revision
     under the rule a publication-v1 broker applies (Trust);
  2. the provider and Delivery accept a member whose `sourceRevision` is the
     plan's, for which the release sends no member evidence, and whose stored
     artifact provenance names the pull request's commit;
  3. an artifact a pull-request channel published stays retained after that
     channel closes, once another channel's head references it.
- A reused member's version keeps the reuse publication's ordered suffix, so
  its commit part names the pull request's commit.
- A publication-v1 broker accepts a reused member as a member carried by the
  resolved head of a channel the plan does not advance, rebound to the plan's
  revision and to no tree or the run's one tree (Trust).
- The publication sends no image evidence for a reused OCI member: evidence
  covers the members it built.
