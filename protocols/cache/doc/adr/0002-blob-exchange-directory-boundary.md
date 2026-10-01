# ADR 0002: Blobs cross the provider boundary through an exchange directory

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/cache` (`protocols/cache`)

## Context

The provider RPC is JSONL over the subprocess's stdin and stdout, one message
per line, correlated by ID. A restore can materialize hundreds of files and an
upload can stream multi-megabyte gzip blobs; bytes in the line would make every
control message proportional to the artifact. Core's content-addressed store
has its own write discipline (atomic stage-and-rename, a GC lock, the
`blobs`↔`cas` hardlink contract) that a second writer would turn into a
cross-repository agreement.

## Decision

Control messages carry metadata only. Bytes move through a content-addressed
blob-exchange directory named in `InitializeParams.BlobExchangeDir`, with
per-blob paths from `BlobExchangePath(exchangeDir, digest)`
(`<exchangeDir>/<hex[:2]>/<hex>`).

- `restore` downloads a hit's blobs into the exchange directory and returns only
  the `Manifest`; `upload` reads blobs core exported there, and its request
  carries only the `Manifest`.
- The exchange directory is the provider's only filesystem contact point, and it
  is not core's live CAS. Core ingests downloaded blobs into its store, exports
  built blobs to the exchange, and remains the sole writer of its store. The
  provider creates its object-cache socket directly under the exchange
  directory, never in a subdirectory: clients derive the exchange directory as
  the socket's parent.
- An exchange directory on the store's filesystem lets core ingest and export by
  hardlink, with no byte copy.

## Rejected alternatives

- **Inline blobs in JSONL.** Control lines grow with the artifact, streaming is
  lost, and peak memory doubles. Inline transfer exists only in the size-bounded
  HTTP `upload-batch` / `download-batch` accelerators (`MaxInlineBlobBytes`).
- **Let the provider use core's CAS directly.** Store atomicity, locking, and GC
  would become a contract between independently released repositories, and a
  provider bug could corrupt the local cache.
- **A socket or shared-memory side channel for blobs.** A second transport to
  configure, secure, and clean up, for data whose natural unit is already a
  content-addressed file.
- **Core fetches bytes from provider-returned URLs.** It puts the HTTP client
  back in core.

## Consequences

- A provider must write to the local filesystem; a purely networked provider is
  not expressible.
- Core owns exchange-directory hygiene (cleanup, crash residue, disk pressure).
- An exchange directory on another filesystem still works, but hardlink handoff
  silently degrades to a byte copy.
- A provider crash can lose an upload or a restore, never corrupt the store,
  which is what lets every op be best-effort.
