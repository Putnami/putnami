# ADR 0001 — The manifest travels inside the bundle, under one digest

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/sitecontent` (`protocols/sitecontent`)

## Context

A site content bundle crosses repositories: one produces documentation, another
serves it, pinned by digest for reproducibility and so a bad publish cannot
change what an earlier build served. The manifest declares the URL prefixes,
the exact file list, and a digest per file, and every path rule the consumer
enforces is checked against it. The manifest is therefore inside the trust
boundary.

## Decision

The bundle is one publishable `tar.gz`, and `bundle.json` at the archive root
is part of it.

1. **One digest addresses manifest and payload.** A payload with a separately
   stored manifest is not a valid bundle.
2. **`bundle.json` is a reserved carrier.** It must be a regular file at the
   archive root and is exempt from the files and mount rules.
3. **The consumer verifies before merging:** extract `bundle.json`, gate on the
   `formatVersion` major, verify every listed digest and that every payload file
   is listed, reject traversal, symlinks, and files outside a declared mount,
   then validate mount ownership. Any failure merges nothing.
4. **Registry metadata is provenance.** The `{repo, commit}` source block is
   for reviewers; verification never trusts it.
5. **Any further metadata document** goes inside the archive under the same
   digest.

## Rejected alternatives

- **Manifest as a separate registry artifact.** Needs its own pin and fetch,
  and a mismatch is unverifiable.
- **Registry-served manifest metadata.** Puts the registry inside the trust
  boundary and ties the contract to one registry.
- **Derive the manifest at consume time.** A new payload directory would
  silently claim new URLs; the publisher must declare its URL space.
- **Detached signature.** Solves authenticity, not which rules apply, and adds
  key distribution.

## Consequences

- Changing mounts or files means republishing and re-pinning; the committed
  lock diff is the review surface.
- `bundle.json` is unavailable to payloads at the root, and every
  implementation special-cases it.
- One corrupt file fails the whole update; the consumer's baked fallback keeps
  that from being an outage.
