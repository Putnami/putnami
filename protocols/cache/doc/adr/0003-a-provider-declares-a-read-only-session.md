# ADR 0003 — A provider declares a read-only session at initialize

- **Status**: accepted
- **Date**: 2026-11-14
- **Scope**: `go.putnami.dev/protocol/cache` (`protocols/cache`): the
  `read-only` capability of the provider RPC, and the CLI's upload path

## Context

A cache credential that cannot write is common: a workspace machine token, or
a developer's own credential, which the cache server lets read and not write.
The provider knew it, and answered every upload `accepted: false` with no
error. The CLI still exported each entry's blobs, sent the upload, and logged
`provider upload failed ... error=<nil>` once per job, then summed the
declines as upload errors. A developer credential learned it could not write
only from the first refused write.

## Decision

1. **Core lists `read-only` in initialize.** A provider echoes it when its
   session cannot write, and only when core listed it. The capability adds no
   field, so a strict parser that predates it reads the same bytes; an unknown
   capability string is ignored on both sides.
2. **After the echo core writes nothing.** It exports no blob, sends no
   upload and publishes no run marker for the session, and prints one line
   that the remote cache is read-only for this credential. Reads are
   unchanged.
3. **A decline is not an error.** An upload answered `accepted: false` with no
   error is declined: core logs it at debug level and does not count it as an
   upload error.

## Rejected alternatives

- **A new `writable` field on the result.** A field moves the strict wire; a
  capability string does not, and it follows the opt-in pattern of
  `restore-result-only` and `provider-configuration`.
- **Let core infer read-only from the first decline.** The first upload still
  pays the export, and the provider already knows the answer at initialize.

## Consequences

- A provider that cannot tell at initialize keeps declining uploads; the
  declines no longer read as failures.
