# ADR 0011 — Streamed octets carry their own media type

- **Status**: accepted
- **Scope**: Go API, OpenAPI, HTTP and client; TypeScript application and client

## Context

A blob upload streams an archive to storage, and the download replays its
stored media type. A fixed media type and a mandatory in-memory buffer cannot
express that. Removing the bound from `Binary`
([ADR 0007](0007-a-raw-octet-payload-is-declared-and-bounded.md)) would turn a
bounded allocation into an unbounded one.

## Decision

**Declaration.** `BinaryStream(maxBytes)` declares one raw, unframed HTTP body
on a unary REST operation. Its OpenAPI media entry is `*/*` with
`schema: {type: string, format: binary}`, `x-putnami-streamed: true` and a
strictly positive `x-putnami-max-bytes`. The neutral IR carries
`streamed: true` and `maxBytes`. It is never an SSE, Connect or WebSocket
message stream.

**Media type.** The sender supplies a valid concrete Content-Type. Empty,
malformed and wildcard labels are refused before reading; parameters are kept
unchanged. Every label, JSON included, describes opaque octets: the body never
enters a document codec. The wildcard describes label selection, not content
negotiation.

**Go and TypeScript surfaces.** Go handlers read an `io.Reader`
(`api.BinaryStreamBody`) and answer with `BinaryStreamResponse`. Generated Go
inputs carry `ContentType` and `Body io.Reader`; outputs carry status, content
type and `Body io.ReadCloser`, which the consumer closes. TypeScript returns a
`ReadableStream<Uint8Array>` and accepts the binary-source union for requests.
Both keep incremental consumption, cancellation and byte identity.

**No replay.** An upload reader is single-use: no retry, redirect or
credential-remint replay consumes it twice. Authentication happens before
sending. Response readers keep their request deadlines until EOF, error or
close. Error envelopes stay bounded and typed. A response cache on a streamed
operation is refused.

**Bound.** The provider applies the declared bound to that route only and
answers the declared 413 when `Content-Length` or incremental consumption
exceeds it; the server-wide JSON limit is unchanged. A handler that needs the
refusal to carry 413 consumes the request before sending response headers;
after headers, an overflow terminates the response body. The provider also
declares the media-type refusal.

**Go request scope.** Request transactions finalize before response headers,
so a failed commit still answers 500. Scoped resources stay alive through the
streamed copy, and a source must not depend on an uncommitted transaction
after its handler returns. Response sources are closed serially after
copying; a source that needs prompt cancellation while blocked in `Read`
watches the request context.

## Consequences

- No bounded call becomes unbounded. Older strict readers reject the new
  extension instead of guessing a buffer policy.
- The OpenAPI generated-client test calls a real provider and proves the
  provider sees the upload prefix before the consumer supplies its suffix.
