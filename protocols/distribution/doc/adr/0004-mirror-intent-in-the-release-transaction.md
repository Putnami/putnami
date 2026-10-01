# ADR 0004 — Mirror intent in the release transaction

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/distribution` (`protocols/distribution`),
  `@putnami/cli`

## Context

A repository declares `distribution.registries.<ecosystem>.mirror.to` in
`putnami.ci.json`. A copy started only after the provider answers a release
disappears when the provider crashes, and a caller retrying an
already-current set cannot recover the lost intent.

## Decision

1. `ReleaseRequest` carries optional
   `mirrors: {"<ecosystem>": {"to": "<destination>"}}`, keyed by the generic
   ecosystem vocabulary, at most 16 entries. A destination is an ASCII registry
   host/path or HTTPS URL, 1 to 2048 bytes, without userinfo, query, fragment
   or whitespace. Strict parsing rejects null entries and unknown or duplicate
   fields. A rejected destination is never echoed in a diagnostic.
2. The field conveys intent and never grants access. The provider validates
   each destination against its configured registry adapters and authorized
   targets, and resolves a permitted spelling to its canonical target identity
   before deduplication. Explicitly configured aliases share that identity;
   URL normalization is not an authorization mechanism. No credential travels
   in this protocol.
3. The CLI carries the declared map in its single release operation. An entry
   whose ecosystem has no member in the set creates no copy intent. Only
   members whose stored visibility is `public` after resolution are eligible.
4. A `released` or `already-current` answer requires durable copy intent
   written in the same transaction as release acceptance. A `conflict` creates
   none. Acceptance does not confirm that a copy completed: the provider
   exposes pending, completed and failed copies on its operator surface, and
   `channel-status` describes native registry generations only.
5. A copy is identified by the member identity, its digest and the authorized
   destination. A retry of the same tuple is idempotent; a different
   destination is distinct work. Workers copy the accepted bytes at that exact
   version through the provider's durable queue and never rebuild.
6. Mirror intent is not part of the canonical set, its digest or a channel
   head, so an already-current release can request a missing copy without
   rebuilding or moving a channel. Omitting the field or sending an empty map
   requests no new copy and never revokes accepted work. `channel-set` carries
   no mirror destinations.

## Consequences

- The provider and the CLI move together: a strict provider without the field
  rejects a nonempty map, and a caller must never retry by silently dropping
  it.
- Durable copy storage, workers and their qualification belong to the
  provider.
