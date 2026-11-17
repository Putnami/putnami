# ADR 0099 — A retry adopts its commit's stored version

- **Status**: accepted
- **Date**: 2026-11-17
- **Amends**: [ADR 0076](0076-publication-uploads-leave-the-engine.md),
  decision 3 (the answer must be the packed artifact)
- **Scope**: `@putnami/cli` (`internal/jobs/publication`), the
  `uploader-v1` answer (`go.putnami.dev/protocol/registry`, one more member),
  the SDK upload kit (`publicationupload`, `putpublish`), features
  `cli/provider-publication`, `registry/credential-seam-contract` and
  `tooling/extension-authoring`

## Context

A clean build versions a put or archive member
`0.0.0-<commit time>-<short commit>`. The Put registry keeps a version
immutable and answers 409 to a second publish with other bytes. An archive
is not byte-reproducible: two builds of one commit pack other bytes. When a
publish fails after some members were uploaded, a retry of the same commit
rebuilds them and every one of those members conflicts. Under ADR 0076 the
answer must name the packed digest, so the retry cannot pass, and no later
attempt of that commit can either.

## Decision

1. **A version names a revision.** It does when its last hyphen-separated
   identifier is a lowercase hexadecimal prefix of at least 7 characters of
   that revision (`registry.VersionNamesRevision`).
2. **The provider may adopt.** When a put or archive member's version names
   the plan's source revision and the registry already holds that version
   with another manifest of the same media type, the provider answers
   `adopted: true` with the stored digest and, for an archive, the stored
   platforms. It reads the stored manifest before it uploads any blob.
3. **The engine accepts an adoption only where it holds.** It releases the
   answered digest for a put or archive member whose version names the
   plan's source revision. An archive answer must name the platforms its
   packed manifest names, at the stored digests; a put answer names none.
   Any other adopted answer fails the member.
4. **No other ecosystem adopts.** An OCI version is its digest and its tags
   move, so a rebuilt image never conflicts. A Go module zip is
   reproducible. npm is left out: its probe cannot read the stored digest
   when the sizes differ.

## Consequences

- A retry of a commit whose members were already published passes publish,
  and releases what the first attempt stored.
- For an adopted member, the engine releases a digest it did not hash. It
  trusts the provider for it, as it already trusts the provider to upload
  the bytes it hashed. The guard is the version: only the commit that built
  the stored version can adopt it.
- Any other conflict is still `artifact_conflict`, and its message says a
  retry of this commit cannot pass.
- An engine older than this decision refuses an answer that carries
  `adopted`.
