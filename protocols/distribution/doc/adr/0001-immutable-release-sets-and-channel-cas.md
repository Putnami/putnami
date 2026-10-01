# ADR 0001 — Immutable mixed-version release sets behind channel CAS

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/distribution` (`protocols/distribution`)

## Context

A sparse publication cannot give every artifact one commit-derived version.
When only a downstream library changes, rewriting its unchanged upstream to the
new version references an artifact nobody published; republishing everything
discards impact selection. A publication also produces images, archives,
configuration and migrations, and deploy must consume one record that names
all of them.

A channel is mutable by design, while a publish result and a deployment input
must stay stable. Resolving each artifact tag separately lets a channel move
assemble versions that never belonged to one publication.

The contract crosses repositories. If each one spells member structures,
ordering or hashing on its own, identical sets get different identities. The
module must still know no Cloud host, credential or storage. New ecosystems
(Python, Java) must join without a protocol change.

## Decision

### 1. One immutable full snapshot, with mixed member versions

`distribution/release-set/v2` (`protocolVersion: 2`, the only version) is a
full namespace snapshot. A member is keyed by `(ecosystem, coordinate)` and
carries its own exact `version`, `artifactDigest`, and exact internal
`dependencies`. Unchanged members are inherited from the channel head while
selected members and their downstream are repackaged. Validation rejects a
dependency edge whose target is absent or whose version differs from the
target member, and rejects duplicate keys.

A project yields any number of members in one or several ecosystems; the
project is provenance, never identity
([ADR 0005](0005-member-attribution-project-and-kind.md)).

### 2. The protocol keeps only the generic part of an ecosystem

`ecosystem` matches `^[a-z][a-z0-9-]{0,31}$`. `coordinate` and `version` are
opaque, bounded, control-character-free strings. `platforms` (os/arch to
SHA-256 digest) is optional for every ecosystem. The extension that owns the
ecosystem profile validates coordinate and version shapes at plan time; the
registry validates at write time; this module validates neither. Mutable
selectors are never a version.

### 3. Members carry provenance; the baseline is the channel head

Every member records `sourceRevision`, the full commit it was published from,
and `selectionFingerprint`: the engine's deterministic execution key of the
member's package step with the embedded version omitted, computed even when
cache restoration is disabled. A publication compares each member's working
tree fingerprint with the channel head's member and republishes on difference,
whatever the git history in between. CI gaps and rebases therefore converge on
the same set.

### 4. Identity is the SHA-256 of one canonical projection

The hashed projection holds `protocolVersion`, `namespace` and the members.
Members and dependencies sort by `(ecosystem, coordinate)`; fixed struct field
order is serialized as JSON with no insignificant whitespace or trailing
newline; empty optional fields are omitted. For SHA-256 hex `h` the reference
is `{id: "rs_" + h, digest: "sha256:" + h}`. The reference is never part of
the hashed document. Canonicalization returns a deep copy and never mutates
caller state. `ParseCanonicalReleaseSet` accepts only the exact canonical
bytes.

### 5. Channels

- A channel name matches `^[a-z0-9][a-z0-9._-]{0,63}$`, valid at once as an npm
  dist-tag, a Go query, an OCI tag and a put channel. A channel derived from a
  git tag encodes `/` as `-`.
- The mutable `(namespace, channel)` head points to one immutable ref and
  carries a monotone `generation`, 1 for the first accepted head. A registry
  applies a projection only when the generation increases, so a delayed event
  never restores an older projection and retries are idempotent.
- A channel requested with `immutable: true` accepts `expected: null` once and
  refuses every later move. A tagged publish uses it.
- A channel the repository declares `protected` in `putnami.ci.json`
  (`protocols/ci`) is refused by `publish` and by every rule; only
  `channel set` by a user moves it. The provider enforces the principal check;
  protection is not carried on this wire.
- Grants are a provider concern: a grant on a namespace, or on a namespace and
  channel, covers every version the channel has named since the grant.

### 6. Four provider operations

An extension advertises the reserved command `cloud-release-set`; the wire is
`putnami cloud release-set <op> --request-file <path>`. The module owns
request and response shapes and exchange validators, so a consumer cannot skip
recomputing a ref. It implements no discovery, subprocess, transport, storage,
authorization or channel concurrency.

- **`resolve`** takes exactly one selector: `channels` (1 to 16 names) or
  `releaseId` (`rs_<64 lowercase hex>`, never a digest, never through a
  channel field). A channel answer is `heads`, one entry per channel, `null`
  for an empty channel (a complete answer, not a failure). A release-id answer
  is `release` with generation 0 and the requested id. The answer names every
  requested channel and no other; the validator recomputes each set's ref and
  binds the namespace.
- **`release`** carries the set, `channels: [{name, expected, visibility,
  immutable}]` and the repository's visibility chain. The provider stores the
  set and compare-and-swaps every listed channel from its own `expected`
  (`null` asserts absence) in one transaction. The outcome is `released`,
  `already-current`, or `conflict`. A conflict writes nothing and reports
  every current head; the publication coordinator treats it as a failed
  publication and replans. There are no typed projections: each registry
  knows what a channel means for it.
- **`channel-set`** is a metadata-only move: target channel, its `expected`
  head, and a source `{channel}` or `{releaseId}`. The provider re-releases the
  existing set with the guarantees of `release` and uploads nothing.
  Promotion and rollback are this operation.
- **`channel-status`** returns `desired` (the accepted head and generation)
  and `observed` (per registry, the last generation applied). A publisher that
  finds no subscriber reports it instead of counting a silent zero.

Publish output (`ReleaseSetPublishOutcome`) carries the exact ref and the
per-channel heads the provider accepted. Deploy keeps that ref and never
resolves the channel again, so a channel move between publication and
deployment cannot change what is deployed.

### 7. Visibility is resolved by the provider, per member, at release

The chain declared by the repository, repo > registry > channel > version
(stable, prerelease) > set > member, travels with the request; the finest
level that states a value wins and a silent level inherits. Levels are ordered
`internal` < `private` < `public`. The CLI resolves member selectors into
member coordinates before sending the request, and computes no level; the
provider never sees a project selector. The provider records, per artifact
version, the widest level ever resolved and never narrows it. Registries keep
that value on the version and enforce it.

## Rejected alternatives

- **One version for the whole release.** A downstream-only publish would name
  an unpublished upstream, or republish it for nothing.
- **Hash the authored bytes.** Ordering or whitespace would give equivalent
  snapshots different identities.
- **Include id/digest in the hashed document.** Self-referential, and admits a
  disagreement between asserted and computed identity.
- **Channels per artifact.** Independent resolution assembles a snapshot no
  publication produced.
- **Advance without an expected head.** Concurrent publishers overwrite each
  other and report success from stale plans.
- **Resolve the channel at deploy.** The deployed bytes would no longer be
  proven by the publish result.
- **Store and advance as two calls.** A crash between them leaves a stored set
  no channel names.
- **Overload `channel` with a release-set id.** A mutable and an immutable
  selector need distinct validation and audit semantics.
- **A closed ecosystem enum with per-ecosystem rules in the protocol.** Every
  new ecosystem would need a protocol version.
- **Copy the structs or canonicalizer into the provider repository.** One byte
  of drift yields incompatible identities.

## Consequences

- Canonical bytes and the ref are public compatibility surface. A change to
  field interpretation, ordering, hashing or closed outcomes needs a new
  protocol version and a staged cross-repository rollout. An additive optional
  member field keeps every existing ref only when omitted when empty.
- Adding an ecosystem changes nothing here: an extension declares the profile,
  a publish job emits the member, a registry stores the artifact.
- The provider implements `release` and `channel-set` transactionally. It
  imports an exact module version and runs the embedded conformance corpus
  and golden (`EmbeddedGolden`); it never copies the wire types.
- Rollback selects an earlier immutable ref; it never rebuilds or replaces the
  bytes of an existing set.
- `protocols/runtime` describes the release step's result data as the
  `ReleaseSetPublishOutcome` with one head per advanced channel.
