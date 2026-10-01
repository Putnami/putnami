# ADR 0002 — Enforce bucket constraints on the plugin backend, and never delete on a rejected write

- **Status**: accepted
- **Scope**: `go.putnami.dev/storage` (`go/framework/storage`)

## Context

`WithMaxFileSize` and `WithAllowedMimeTypes` must hold for the idiomatic
`storage.NewPlugin` composition. Deleting the key after an oversized write
destroys the object that write tried to replace.

## Decision

1. **Backend contract.** A `Put` whose body reader fails commits nothing and
   leaves any existing object and its metadata intact. The filesystem backend
   writes a temporary file in the object's directory and renames it over the
   object once the body is complete, then replaces the metadata file the same
   way; both keep the mode `os.Create` gives. It reserves key segments ending
   in `.meta.json` or `.putnami-tmp`. The memory backend reads the whole body
   before storing. S3 and GCS abort the request, so the provider commits
   nothing.
2. **No delete on a reported rejection.** A body of known length is checked
   before any backend I/O. Within the limit, the backend still sees the
   length, and the wrapper holds back the last byte until the source is proven
   to end there, so a source that grows fails before an HTTP backend has the
   whole body; growth after that proof is not read. Other bodies stream
   through a size guard. When a reader refuses the body and the backend
   returns an error, the wrapper returns its rejection with the backend error
   as cause and deletes nothing. When the backend reports success after a
   refused read, it committed a truncated object this `Put` wrote; the wrapper
   deletes it best-effort and records a failed delete in the `cleanupError`
   attribute.
3. **Enforced by default on the plugin backend.** `Plugin` provides a
   `ConstrainedBackend` over the binding layer, enforcing registered bucket
   constraints on logical names. `ConstrainedBackend` implements `URLSigner`
   by delegation and fails with `storage.unsupported` when the wrapped backend
   cannot sign. Its `SignedPutURL` rejects a content type missing from the
   bucket's MIME allowlist, including an empty one; the S3 and GCS signers bind
   that content type into the signature. A signed PUT bypasses the server, so
   `MaxFileSize` does not apply to it.
4. `NewConstrainedBackend` enforces constraints on a backend you construct
   yourself and keeps its signed URLs. A raw backend enforces none.

## Rejected alternatives

- **Delete, then restore on failure.** It copies every replaced object, and a
  crash between delete and restore still loses data.
- **Refuse signed PUT URLs on sized buckets.** It removes direct uploads;
  providers bound size only through signed POST policies, which `URLSigner`
  does not model.

## Consequences

- A backend implementation must honor the `Put` contract; one that reports
  success after a failed read loses the truncated object, not the previous
  one.
- An upload through a signed PUT URL can exceed `MaxFileSize`; check the
  stored size when the limit matters.
