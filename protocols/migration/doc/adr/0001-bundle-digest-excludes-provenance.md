# ADR 0001: The bundle digest addresses migrations, not the build that made them

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/migration` (`protocols/migration`)

## Context

A migration bundle is published once and executed later, possibly from another
machine, checkout, or pipeline. Publish must be idempotent by content:
re-publishing the same migrations must address the same artifact. The manifest
also carries provenance a human needs (`version`, `git`, `imageDigest`,
`generatedAt`, `source`), which differs on every build by design.

## Decision

`ComputeBundleDigest` normalizes the bundle, sorts operations canonically (kind
→ target → namespace → orderKey → name, hashes breaking ties), and hashes only
`protocol`, `appName`, and `operations`.

- `version`, `git`, `imageDigest`, `generatedAt`, `source`, and the digest
  itself are excluded, so the same migrations built from different checkouts or
  releases produce the same digest.
- Payload references are part of `operations`, and payload hashes are
  lowercase SHA-256 hex like `Definition.Hash`, so a changed migration body
  changes the digest even under the same file name.
- The digest is the identity for remote publish idempotence and for the test
  provider's `bundle-template` reuse, which keys a migrated template database by
  it.
- The algorithm is byte-identical across languages, pinned by the shared golden
  `fixtures/equivalence/bundle.golden.json` asserted from Go and TypeScript.

## Rejected alternatives

- **Hash the whole manifest.** `generatedAt` alone makes every build a new
  artifact; template reuse never hits.
- **Drop provenance from the manifest.** Provenance makes a published bundle
  auditable and costs nothing outside the digest.
- **Hash the payload tree on disk.** The digest would depend on layout and
  archive details, and could not be computed before materialization.
- **Per-language digests, bundles compared by name and version.** Two different
  migration sets can share both labels.

## Consequences

- Bundles with identical migrations and different provenance are the same
  artifact; a consumer that must tell them apart reads the provenance fields.
- Changing the hashed subset, the operation order, or normalization breaks every
  published digest: both implementations and the golden move together.
- `VerifyBundlePayloads` detects a tampered or missing payload independently of
  the digest; publish tooling parses, validates, and verifies before upload.
