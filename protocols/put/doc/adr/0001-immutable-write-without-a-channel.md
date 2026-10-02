# ADR 0001 — Immutable write without a channel

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/put` (`protocols/put`)

## Context

Under `publication-v1` the engine uploads every managed release-set member in
its own process and only the provider's `release` moves channels
([registry ADR 0003](../../../registry/doc/adr/0003-publication-ops-behind-a-negotiated-capability.md)).
Put registry members (release archives, config members, migrations and
site-content bundles) were written by jobs of another repository, each with
its own copy of the request shapes, and a site-content publish moved a channel
in the same call.

## Decision

1. **The write is the registry's immutable subset.** A publish uploads the
   referenced blobs, then sends `{version, media_type, payload}` to
   `/{namespace}/{package}/publish`. It carries no `channel`, `visibility` or
   `source_ref`, so the version is private and no channel moves. A release-set
   provider moves channels at `release`.
2. **The digest is computable before the upload.** The member digest is the
   SHA-256 of the stored manifest payload. A payload must already be in the
   form the registry stores (compact, HTML-escaped `encoding/json`), so the
   stored bytes equal the sent bytes.
3. **A conflict is accepted only by readback.** A 409 is followed by an
   authenticated manifest read; the version is accepted only when the media
   type and the payload bytes equal the publisher's.
4. **The media types are a closed table per member kind.** `config`,
   `migration`, `doc` and `archive` each publish one manifest media type and a
   fixed set of blob media types.
5. **A manifest references exactly the blobs the publisher uploads.** The
   references are the members the registry links: `blob_digest`,
   `artifact.blob` and `artifacts.<key>.digest`.
6. **Every message decodes strictly.** An unknown member, a duplicate member
   and trailing data are refused on both sides.

## Rejected alternatives

- **The registry's full publish request, with `channel`.** A channel move
  outside `release` is a second writer of channel state.
- **A digest the registry reports.** A publisher could not check the digest it
  plans against the digest it publishes.
- **Lenient decoding of answers.** A client would accept a version shape it
  does not understand.
- **Data's migration API for a migration member.** A second, private API and a
  second credential in the engine. The provider runs Data's acceptance at
  `release` instead.
