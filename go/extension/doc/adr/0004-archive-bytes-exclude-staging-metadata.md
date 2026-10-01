# ADR 0004 — Archive bytes exclude staging metadata

- **Status**: accepted
- **Scope**: `@putnami/go` extension and template packaging

## Context

An interrupted immutable publication can leave accepted members before the
release set commits, so a retry must reproduce their digests. Staging
timestamps and host ownership in the tar headers change the digest without
changing a file byte, and immutable manifest readback then refuses the
rebuilt archive.

## Decision

Extension and template archives use the Go tar and gzip writers. Staged
entries are walked in lexical order. Modification times are the Unix epoch;
access and change times and owner names are omitted; owner IDs are zero. File
contents, permissions and symbolic-link targets are preserved. Unsupported
special files are rejected, and an incomplete output is removed after a write
or close failure.

## Consequences

Identical staged inputs produce identical archives across retries. Compiler
output is not promised to be reproducible: a changed binary still changes the
digest. Registry collision checks keep their exact-byte requirement.
