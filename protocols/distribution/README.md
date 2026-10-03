# Distribution release-set protocol

`go.putnami.dev/protocol/distribution` is the sole authority for
`distribution/release-set/v2`: immutable, coherent snapshots of every artifact a
publication produced, the channels that name them, and the visibility they are
released under.

The design is fixed in
[ADR 0001](doc/adr/0001-immutable-release-sets-and-channel-cas.md).

## Release set

A release set is a full snapshot in one namespace. A member is keyed by
`(ecosystem, coordinate)`: one project yields any number of members, in one or
several ecosystems, and the project is provenance rather than identity.

```json
{
  "protocolVersion": 2,
  "namespace": "putnami",
  "members": [
    {
      "ecosystem": "oci",
      "coordinate": "putnami/sites/putnami.dev",
      "version": "0.3.0-8d5edb751",
      "artifactDigest": "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
      "dependencies": [
        {
          "ecosystem": "npm",
          "coordinate": "@putnami/web",
          "version": "0.3.0-8d5edb751"
        }
      ],
      "sourceRevision": "8d5edb7513d93b9165ba2a7cb48466d022fc3f63",
      "selectionFingerprint": "sha256:4444444444444444444444444444444444444444444444444444444444444444",
      "platforms": {
        "linux/amd64": "sha256:6666666666666666666666666666666666666666666666666666666666666666"
      },
      "project": "sites/putnami.dev",
      "kind": "image",
      "sourceTree": "c0ffee5f1e2d3c4b5a69788796a5b4c3d2e1f0a9"
    }
  ]
}
```

### The protocol keeps only the generic part of an ecosystem

`ecosystem` is an identifier matching `^[a-z][a-z0-9-]{0,31}$`. `coordinate` and
`version` are opaque, bounded, control-character-free strings. `platforms` is
optional for every ecosystem. Python or Java joins without a protocol change:

- the extension that owns the ecosystem profile validates coordinate and version
  shapes at plan time (`protocols/extension`);
- the extension metadata block that declares a member supplies that member's
  package and publish jobs; it may use a profile owned by another extension;
- the registry that stores the artifact validates at write time;
- this module validates neither.

What this module does enforce: bounds, `sha256:<64 lowercase hex>` digests, the
40-hex `sourceRevision`, the `sha256:` `selectionFingerprint`, `os/arch`
platform keys, the optional `project` and `kind` attribution and the optional
40-hex `sourceTree` described below, uniqueness by `(ecosystem, coordinate)`, and a dependency closure that resolves
inside the same set at exactly the declared version.

`selectionFingerprint` is derived from the deterministic execution key of the
member's declared package step, with its embedded version omitted. That key
exists even when the task deliberately disables cache restoration. A member
whose fingerprint equals the working tree's is inherited; any other is
republished, whatever the git history in between. A change of packager or of an
upstream dependency therefore republishes, without any typed cross-ecosystem
dependency.

### Attribution: which project produced a member, and what it is

Two optional fields let a consumer holding only the set answer "which member is
workload X's image, which is its config, which are its migrations" without a CI
run in the loop:

- `project` is the canonical logical id of the project that published the
  member, without a leading slash, matching
  `^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`. It is the identity a selector names,
  which is not always the physical directory path: a workspace may nest a
  project under folders that are transparent to identity, and they are omitted
  here — the grammar cannot express one. It is PROVENANCE, not member identity:
  the member key stays `(ecosystem, coordinate)`, and one project still yields
  any number of members;
- `kind` is the closed role vocabulary `image | config | migration | doc |
  library | archive | deployment`. `deployment` is a workload's deployment
  declaration: its resolved requirements and runtime as one document, published
  in the `put` ecosystem by the package step `deployment`. A publisher that
  cannot classify an artifact records no kind; the protocol never invents one.
  A new token is appended, readers learn it before any publisher emits it, and
  the addition is one-way (ADR 0005, rule 7).

Both are optional, so a set accepted before they existed keeps validating and
keeps deriving the same `rs_` reference: `omitempty` omits them, and an
explicitly empty value decodes as absence, exactly as an empty `platforms` map
does. A present value is validated against its grammar, so a consumer that
selects by role never has to guess what an unknown token meant.

Both are part of the canonical bytes. A set that names the project of each
member is a DIFFERENT immutable set from one that does not, which is correct: it
states more about the same artifacts, and its reference must say so. An
opted-in publisher records both on every member of the set it releases,
carried-over members included: attribution is the plan's statement about a
member, while the rest of an unchanged member's record is inherited from the
head verbatim (ADR 0005, rule 3).

A publisher emits them only when the repository opts in with
`distribution.memberAttribution: true` in `putnami.ci.json`: every consumer
decodes strictly, so the extensions the workspace pins and its release-set
provider must know the fields before they appear (ADR 0005, rule 7).

### Source tree: the content a member was built from

`sourceRevision` names a commit, and a squash-merge or a rebase gives the same
content a new commit. The optional `sourceTree` is the full lowercase hex git
tree of the checkout the member was built from, matching `^[0-9a-f]{40}$`
(`distribution.invalid_source_tree` otherwise). Two members with the same tree
were built from the same content, whatever their revisions say. SHA-256
repositories are out of scope, as they are for `sourceRevision`.

It is provenance like `project` and `kind`: optional, omitted when empty so an
older set keeps its `rs_` reference, and part of the canonical bytes when
present. It belongs to the artifact record, so an unchanged member inherits it
from the head verbatim with the revision it goes with. Whether an equal tree
lets a publication skip republishing is the consumer's decision; the protocol
only records it.

A publisher records it only on a clean checkout, where HEAD's tree is what it
built (it reads the tree again when it commits the set and drops a tree the
checkout no longer holds), and only when the repository opts in with
`distribution.memberSourceTree: true` in `putnami.ci.json`, for the same reason
as attribution: every strict reader must know the field first
([ADR 0005](doc/adr/0005-member-attribution-project-and-kind.md)).

### Identity

`NormalizeReleaseSet` returns a deep copy sorted by `(ecosystem, coordinate)`
and never mutates caller data. `CanonicalReleaseSetBytes` validates that copy
and encodes the fields in the struct order shown above with `json.Marshal`:
UTF-8 JSON, no insignificant whitespace and no trailing newline. Derived
identity is excluded from this hash projection:

```text
hex    = lowercase_hex(sha256(canonical_bytes))
digest = "sha256:" + hex
id     = "rs_" + hex
```

`ParseCanonicalReleaseSet` is for immutable storage boundaries and accepts only
those exact bytes. `ParseAndValidateReleaseSet` accepts equivalent object and
collection order, then returns the normalized value. Both paths reject unknown,
duplicate, missing, or unexpectedly-null fields and trailing JSON.

## Channels

A channel is a pointer from the namespace to a set. Names are portable:
`^[a-z0-9][a-z0-9._-]{0,63}$`, valid at once as an npm dist-tag, a Go query, an
OCI tag, and a put channel. `EncodeTagAsChannel` maps a git tag to the immutable
channel a tagged publish creates, encoding `/` as `-`: `ts/v0.3.0` becomes
`ts-v0.3.0`. `IsPortableChannel` is the predicate.

Every head carries a monotone `generation`, stamped by the provider when it
accepts a move. It starts at 1. An immutable release resolved by id is named by
no channel and reports generation 0.

## Visibility

Three ordered levels: `internal` < `private` < `public`, carried by `Visibility`
with `Valid`, `Rank`, and the ratchet operator `MaxVisibility`. Visibility
attaches to the member and is resolved by the provider, per member, at release,
along the chain the repository declares: repo, registry, channel, version, set,
member. The finest level that states a value wins; a silent level inherits. The
CLI computes nothing: it carries `VisibilityChain` with the request.

The provider records, per artifact version, the widest level ever resolved and
never narrows it. Registries keep that value on the version and enforce it.

## Provider seam

An extension capable of serving the protocol declares the reserved flat command
name `cloud-release-set` (`ProviderCommandName`). Provider selection is outside
this module. The only invocation tokens this protocol defines are:

```text
putnami cloud release-set resolve        --request-file <absolute-path>
putnami cloud release-set release        --request-file <absolute-path>
putnami cloud release-set channel-set    --request-file <absolute-path>
putnami cloud release-set channel-status --request-file <absolute-path>
```

The exported constants pin every token. This package assembles no command,
launches no subprocess, names no host, and contains no credentials or transport
policy.

### `resolve`

Exactly one of `channels` (1..16 unique portable names) or `releaseId`.

```jsonc
// request
{ "protocolVersion": 2, "namespace": "putnami", "channels": ["canary", "staging"] }
// response
{ "protocolVersion": 2, "heads": {
  "canary":  { "ref": { "id": "rs_…", "digest": "sha256:…" }, "generation": 41, "releaseSet": { "…": "…" } },
  "staging": null
} }
```

`heads` carries one entry per requested channel. `null` is a complete answer —
the channel has no head — distinct from a provider failure, so a first
publication never needs a bootstrap mode and a conflict can never be mistaken
for an empty channel. A `releaseId` request answers with `release` instead, and
never resolves to nothing. Every present head carries its full snapshot, and its
`ref` must recompute from that snapshot's canonical bytes.
`ValidateResolveExchange` binds the answer to the request: exactly the requested
names, the requested id, the requested namespace.

A publication resolves in one call every channel it advances AND the one channel
it only reads: the head a first publish into an empty channel measures against
(D14 as amended by [ADR 0006](doc/adr/0006-a-publication-measures-against-a-channel-it-does-not-advance.md)).
That baseline channel is a requested name here and nowhere else — it is never a
`release` channel, so nothing advances it — and the bound of 16 names covers the
whole list, advanced and read together.

### `release`

One transaction: store the set and advance every listed channel by
compare-and-swap from its own `expected`. A conflict on any channel writes
nothing and names every head.

```jsonc
// request
{
  "protocolVersion": 2,
  "namespace": "putnami",
  "releaseSet": { "…": "canonical set" },
  "channels": [
    { "name": "canary",    "expected": { "id": "rs_…", "digest": "sha256:…" }, "visibility": "internal" },
    { "name": "ts-v0.3.0", "expected": null, "visibility": "internal", "immutable": true }
  ],
  "visibility": {
    "repo": "internal",
    "registries": { "npm": "internal", "oci": "internal" },
    "versions": { "stable": "public", "prerelease": "internal" },
    "set": null,
    "members": [{ "ecosystem": "npm", "coordinate": "@putnami/web", "visibility": "public" }]
  }
}
// response
{ "protocolVersion": 2, "outcome": "released", "current": {
  "canary":    { "ref": { "id": "rs_…", "digest": "sha256:…" }, "generation": 42 },
  "ts-v0.3.0": { "ref": { "id": "rs_…", "digest": "sha256:…" }, "generation": 1 }
} }
```

Outcomes are `released`, `already-current`, and `conflict`. A release answer
reports pointers only; the caller already holds the set. `expected: null` asserts
that the channel has no head, and an `immutable: true` channel accepts that once
and refuses every later move.

`ValidateReleaseExchange` enforces the semantics: `current` names exactly the
requested channels; both success forms report the submitted set as the head of
every one of them with a generation of at least 1; a set already expected on
every channel is `already-current`, never `released`; a conflict must name at
least one head that differs from what the caller expected.

A member entry of the visibility chain must name a member of the released set:
a rule with nothing to apply to is a caller error. The CLI resolves the
repository's project selectors into member coordinates before sending; the
provider never sees a project selector.

### `channel set`

A metadata-only move. The provider re-releases an existing set on the target
channel with the same guarantees as `release`, without any artifact upload.
Promotion and rollback are this one operation.

```jsonc
{ "protocolVersion": 2, "namespace": "putnami", "channel": "latest",
  "expected": { "id": "rs_…", "digest": "sha256:…" }, "from": { "channel": "ts-v0.3.0" } }
```

`from` is exactly one of `{channel}` or `{releaseId}`. The answer is a
`ReleaseResponse` whose `current` is keyed by the one target channel.
`ValidateChannelSetExchange` rejects missing or additional channels, a
`released` no-op, a false `conflict`, and any successful `{releaseId}` move
whose returned head does not name that exact immutable set. An
`already-current` answer remains valid for an idempotent retry whose original
expectation is now stale.

### `channel status`

Desired versus observed.

```jsonc
{ "protocolVersion": 2,
  "desired": { "ref": { "id": "rs_…", "digest": "sha256:…" }, "generation": 7 },
  "observed": { "npm": 7, "go": 7, "oci": 6, "put": 7 } }
```

`desired` is the head the provider accepted, `null` for an empty channel.
`observed` maps a registry kind — an ecosystem identifier — to the generation it
has applied.

## The consistency contract

- **Atomic acceptance.** The provider accepts the set, every channel head, every
  projection intent, and every member visibility together, or nothing.
- **Monotone generation.** Every head carries a generation that only increases.
  A registry applies a projection only when the generation increases, so a
  delayed event never restores an older projection and every retry is
  idempotent.
- **Desired versus observed.** Registries converge asynchronously from the
  provider's facts. `channel status` exposes the gap, so a publisher that finds
  no subscriber reports it instead of counting a silent zero.

## Publish outcome

The successful runtime payload is the exported `ReleaseSetPublishOutcome`,
carried under `data.releaseSet` by the runtime result event:

```json
{
  "protocolVersion": 2,
  "namespace": "putnami",
  "ref": {
    "id": "rs_<64 lowercase hex>",
    "digest": "sha256:<same 64 lowercase hex>"
  },
  "channels": {
    "canary": {
      "ref": {
        "id": "rs_<64 lowercase hex>",
        "digest": "sha256:<same 64 lowercase hex>"
      },
      "generation": 42
    }
  }
}
```

Every channel names the same immutable set: that is what one release means.
Downstream delivery must retain that exact `{id, digest}`. Resolving the channel
again is a different operation and cannot reproduce the publication proof if the
pointer has moved.

When `publish` is expanded as a same-session prerequisite of `deploy`, the CLI
places a framework-owned `putnami:publish~release-set` node in the visible DAG.
That node depends on every publish task, performs the release, and every deploy
task depends on it. A deploy therefore cannot start before the exact outcome
above exists. The node's executable callback is engine-owned and in-memory only.

## Bounds and diagnostics

Strict parsers reject an input larger than 4 MiB before decoding. A release set
contains at most 4,096 members and each member at most 1,024 dependencies and 32
platform digests. A resolve, a release, or a publication names at most 16
channels; a visibility chain and a channel status report at most 16 registry
kinds. Namespaces are at most 128 UTF-8 bytes, release ids have the exact
67-byte `rs_<64 lowercase hex>` spelling, closed vocabulary tokens 64 bytes,
coordinates 512 bytes, and versions 256 bytes. Validation returns at most 128
diagnostics and ends with `distribution.diagnostics_truncated` when necessary.

Every finding uses `go.putnami.dev/protocol/diagnostic`, a stable
`distribution.*` code from `ValidDiagnosticCodes`, and the offending JSON field
whenever one exists. The wire schemas live under [`schemas/`](schemas/); Go
semantic validation remains authoritative for cross-field uniqueness, exact
closure, matching reference hashes, and exchange binding.

## Conformance

[`fixtures/valid/`](fixtures/valid) exercises a minimal set, a set with one
member per ecosystem, and a project that yields several members.
[`fixtures/invalid/`](fixtures/invalid) pins malformed, unknown, duplicate,
missing, and null fields, an unsupported protocol version, a malformed ecosystem
identifier, bounds, digests, platforms, provenance, duplicate coordinates,
unclosed dependencies, exact-version mismatch, and non-canonical bytes — each
with the exact diagnostic code an external consumer must reproduce.
[`fixtures/equivalence/`](fixtures/equivalence) contains Go-authored and
TypeScript-authored spellings that normalize to the exact bytes and ref embedded
in [`fixtures/golden.json`](fixtures/golden.json).

External consumers call `EmbeddedConformanceFixtures`, `EmbeddedGolden`, or
`RunEmbeddedConformance`; the corpus is embedded in the published Go module, so
a provider repository needs no source-tree path and imports no CLI or framework
package.

Provider implementations must consume an exact published module version, for
example `go get go.putnami.dev/protocol/distribution@<exact-version>`, and record
that pin in their protocol ledger. They must not copy the wire structures,
constants, canonicalization, or fixtures into the provider repository. A rollout
may start only after the pinned module is downloadable independently and its
embedded corpus passes against the provider implementation.

## Ownership, support, and evidence

- **Owner:** `protocols/distribution`
  (`go.putnami.dev/protocol/distribution`), which owns types, constants,
  validators, normalization, canonicalization, schemas, fixtures, and
  conformance helpers only.
- **Support status:** `preview`, recorded in the workspace-root
  [`putnami.support.json`](../../putnami.support.json). The shapes and identity
  are fixed for the coordinated rollout, while the first external provider is
  not yet deployed.
- **Evidence:** strict and bounded parsing tests, field-addressable invalid
  fixtures, canonical bytes/ref goldens, Go/TypeScript-authored equivalence
  fixtures, closed provider outcome tests, schema/version pins, and an embedded
  conformance runner intended for the exact-version external consumer.
- **Not owned here:** Cloud hosts, credentials, HTTP, authentication,
  authorization, storage, channel implementation, registry projection,
  migrations, provider discovery, subprocess lifecycle, or rollout flags.
