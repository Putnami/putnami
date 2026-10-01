# ADR 0002: Stored credentials use a PHC-style versioned digest string

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/keyring` (`protocols/keyring`)

## Context

A stored credential digest (password, device code, keyed MAC) must be
re-readable after its algorithm or parameters change, which requires the string
to name them. The digest is also cross-language: one runtime may hash and
another verify, so "whatever the local library emits" is not a contract.

## Decision

A stored digest is a self-describing PHC-style string with its own version tag:

```
$pbkdf2-sha256$v=1$i=<iterations>$<b64-salt>$<b64-hash>
$hmac-sha256$v=1$<b64-salt>$<b64-hash>
```

- The algorithm set is closed (`pbkdf2-sha256`, `hmac-sha256`). The `v=` tag
  (`DigestVersion`) is independent of the keyring document's `ProtocolVersion`.
- Salt and hash are RFC 4648 §4 standard base64 without padding (`+`/`/`), as
  PHC specifies, not the base64url alphabet this module's JWK parameters use.
  The fixtures exercise both alphabets.
- `ParseDigest` is strict and names the failed rule:
  `keyring.digest_structure`, `keyring.digest_algorithm`,
  `keyring.digest_version`, `keyring.digest_params`, `keyring.digest_encoding`.
  `Digest.String()` is the canonical encoder: `ParseDigest(s).String() == s`.
- Iteration bounds are grammar bounds (positive, sanely capped), not a security
  policy. The hashing implementation owns the minimum work factor.

## Rejected alternatives

- **Bare digests with the algorithm stored elsewhere.** One migration separates
  them.
- **A PHC/Argon2 encoder dependency.** No new external dependencies for wire
  formats; the subset is a few dozen lines and hand-rolling it keeps
  cross-language byte parity testable.
- **base64url everywhere.** JWK must stay JOSE-conformant and the digest
  PHC-conformant.
- **JSON-encoded parameters.** A one-line, self-delimiting string fits a text
  column, logs, and fixtures.
- **Accept any algorithm and let the caller validate.** An open set makes a
  downgrade attack a parsing detail.

## Consequences

- Adding an algorithm updates the vocabulary, the fixtures, and both runtimes
  together.
- Two base64 alphabets in one module are a trap; the grammar, diagnostics, and
  shared vectors state it explicitly.
- A well-formed digest can still be too weak for a deployment; parse success
  proves nothing about the policy floor.
