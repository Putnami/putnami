# ADR 0006 — A publication measures against a channel it does not advance

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/distribution` (`protocols/distribution`),
  `putnami-extension-sdk` (`tooling/extension-sdk/releaseset`), `@putnami/cli`

## Context

`publish --channel a,b` resolves every listed head before the plan, measures
unchanged members against a baseline head, and advances every listed channel
to one set, each from its own resolved head. The head a publication measures
against and the channels it advances are two different things. A workspace
that publishes each pull request into `pr-N` finds `pr-N` empty on the first
run and republishes every member, while the records it should inherit sit
under `canary`, which `main` advances and a pull request must never move.
Naming `canary` in `--channel` in either order advances it.

A selection fingerprint is the package task's execution key without the
version ([ADR 0001](0001-immutable-release-sets-and-channel-cas.md)); it says
nothing about a channel. A `canary` record with a matching fingerprint
identifies the same packaging recipe, which is exactly what the skip needs.

## Decision

1. **A publication may name one baseline channel it reads and never advances**
   (`publish --baseline-channel <c>`, or `rules[].baseline` in
   `putnami.ci.json`). The one resolve names the advanced channels, then the
   baseline. The release advances only the listed channels; the baseline is
   never a `ChannelRequest` and never appears in the release or in
   `ReleaseSetPublishOutcome`.
2. **Precedence, evaluated once per publication:**
   1. the first advanced channel's own head, when it exists;
   2. otherwise the baseline channel's head, when it has one;
   3. otherwise empty: every member is selected.

   The baseline is a pure fallback. The second push of a pull request measures
   against its own first push, and a member `main` moved meanwhile is not
   republished on a branch that did not touch it.
3. **Unchanged members inherit the baseline record verbatim**, wherever it came
   from: version, digest, dependencies, revision, fingerprint, platforms,
   project, kind, tree. The set stays a closed full snapshot, and nothing is
   re-uploaded for an inherited member.
4. **No wire change.** `ResolveRequest.channels` carries the baseline after the
   advanced channels. `MaxChannelsPerRelease` (16) bounds the resolve, so the
   CLI refuses at parse time a publication that advances 16 channels and names
   a baseline.
5. **The plan carries it.** `releaseset.Plan.baselineChannel` names the
   baseline only when it is the baseline actually used, and its head rides in
   `heads` beside the advanced ones. The SDK replays the resolve selector as
   `channels ∪ {baselineChannel}`. A publication that never falls back
   produces the same plan bytes as one that names no baseline.
6. **The session says which head it measured against.** `selection.baseline`
   names the set id, and `selection.baselineSource` is `release-set-head` (the
   advanced channel's own head) or `release-set-baseline-channel`.
7. **Visibility is unchanged.** The provider resolves the chain per member
   under the advanced channel's level, and never narrows a level already
   resolved.

## Rejected alternatives

- **Baseline first whenever one is named.** A member the branch changed would
  be republished on every push instead of once.
- **Both heads, matched per member by fingerprint.** Fewest uploads, but the
  SDK would accept two inheritance sources and a reader could no longer name
  the single head a set derives from.
- **A `readOnly` flag on `ChannelRequest`.** It puts a planning input on the
  release transaction; the baseline never reaching `release` is a stronger
  statement.
- **A bootstrap mode that seeds an empty channel.** The same fallback with a
  second operation and a step a human must remember.
- **A capability gate for older extensions.** The CLI cannot see what an
  installed extension was compiled against. An older SDK refuses a plan that
  carries a baseline, loudly and locally, and meets one only when the fallback
  fires (decision 5).

## Consequences

- The SDK and the CLI ship together; an extension on an older SDK fails a
  publication that falls back to a baseline.
- The ci document owns where the baseline is declared and its placement rules
  ([`protocols/ci` ADR 0004](../../../ci/doc/adr/0004-a-rule-names-its-baseline-channel.md)).
