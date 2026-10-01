# ADR 0001 — Bespoke authenticated write, standard proxy read

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/gomod` (`protocols/gomod`)

## Context

Go publishes by pushing a tag, and consumers resolve through `GOPROXY`. That
cannot express two needs: a private module must not resolve through an
unauthenticated fetch, and a release channel (`latest`, `canary`) is a property
of a publish, not of a version string. Everything else about Go module
consumption (`go mod download`, `go get`, checksum databases, editors) already
works and is outside this repository's control.

## Decision

1. **The write path is bespoke and authenticated.** A publish is
   `POST /{module}/-/blobs/upload` (`application/zip`), then
   `PUT /{module}/@v/{version}` (`application/json`). The bearer comes from the
   [`registry`](../../../registry/README.md) credential seam.
2. **Release is a separate operation.**
   `POST /{module}/@v/{version}/release` makes a published version public. It is
   authenticated, idempotent, one-way, and cannot raise the package's public
   ceiling. Its closed response acknowledges `{module, version,
   visibility:"public"}`. The framework publisher never calls it: no plan shape,
   channel, or other repository-controlled input authorizes visibility, and a
   caller needs an explicit server-owned attestation first. All Go publishes and
   smoke reads stay authenticated.
3. **The read path is the unmodified Go module proxy protocol.**
   `GET /{module}/@v/{version}.{info,mod,zip}` is not modelled here.
4. **Publishing is two-phase and content-addressed.** The registry returns the
   digest of the stored bytes, and the publish body echoes it, so a truncated
   upload fails at the blob step.
5. **The server owns the field names; this module pins them.** `go_mod`,
   `zip_digest`, `dist_tag`, and the release fields `module`, `version`,
   `visibility` are pinned by conformance.
6. **`dist_tag` is validated for presence, never content.** Channel naming is
   registry policy. Omitted means no channel routing; the empty string is never
   emitted.
7. **Both sides decode strictly.** Adding a field to either message breaks v1
   on both sides, so a new capability takes a new endpoint.

## Rejected alternatives

- **Authenticated writes bolted onto the proxy protocol.** Looks standard,
  behaves proprietarily.
- **Single multipart publish.** Loses the content-address round trip and makes
  a large upload retry all-or-nothing.
- **Publisher-asserted digest.** Describes the bytes the client meant to send.
- **Model the read path too.** A second description of a contract Go owns can
  only drift.
- **Closed `dist_tag` vocabulary.** The same set in two repositories.
- **`visibility` on the publish request.** A strict-decode break; a separate
  operation is additive with explicit retry semantics.
- **Authenticated readability as release proof.** A bearer can read a private
  version. Only a release acknowledgement plus a byte-exact anonymous download
  proves public availability.
- **Lenient decoding.** An old client would publish something it does not
  understand.

## Consequences

- The server lives in another repository, so no test here exercises a real
  exchange; the module is `preview`.
- Private modules publish only to a Putnami registry, with no second
  implementation to validate against.
- A Putnami registry must also serve the proxy protocol correctly, which this
  module cannot check.
