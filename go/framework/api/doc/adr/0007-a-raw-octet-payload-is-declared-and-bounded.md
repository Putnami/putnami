# ADR 0007 — A raw octet payload is declared and bounded, never inferred

- **Status**: accepted
- **Scope**: `go.putnami.dev/api`, `go.putnami.dev/openapi`, `go.putnami.dev/client`,
  `@putnami/application`, `@putnami/client`

## Context

Base64 inside a JSON string (`format: byte`) is right for a field in a
document and wrong for a body that is bytes: a third larger, and unbounded,
so the provider buffers whatever a peer sends before it can refuse it.
OpenAPI has `format: binary` for that case, but it has no meaning inside a JSON
document, and two languages inventing an encoding there would disagree.

This record governs buffered `Binary`. Unbuffered bodies with a sender-chosen
media type are [ADR 0011](0011-streamed-octets-carry-their-own-media-type.md).

## Decision

**A raw octet payload is declared with two facts, and the declaration is the
whole contract.**

1. **The media type.** `api.Binary(mediaType, maxBytes)` in Go,
   `Binary({ mediaType, maxBytes })` in TypeScript. `application/json` and every
   `+json` suffix are refused at declaration.
2. **The byte bound, mandatory.** An unbounded octet body forces the provider
   to buffer anything or stop mid-read with no declared answer. A contract
   that declares `format: binary` without a bound is refused, never defaulted.
3. **The projection publishes both.** The media type carries
   `{"type": "string", "format": "binary"}`, and the bound travels beside it as
   `x-putnami-max-bytes`, since no JSON Schema keyword carries it.
4. **`format: binary` is legal only as the root schema of a non-JSON media
   type.** Anywhere a JSON document reaches, it is a generation failure naming
   the operation and the member. `format: byte` stays supported everywhere.
5. **The bound is enforced before the whole payload exists.** The generated
   client reads its source one octet past the bound and refuses there. The
   provider refuses an oversized `Content-Length` before reading, and a lying
   peer from the bounded read. The client caps the 2xx response read by the
   declaration; error envelopes stay readable.
6. **The two refusals are declared errors**, `http.payload_too_large` and
   `http.unsupported_media_type`, added to the operation by the declaration.
7. **Connect is not advertised** for such an operation: its envelope would
   carry the octets as base64. A contract that dispatches octets on Connect is
   refused at generation.
8. **A stream carries no octet payload.** A stream carries declared messages,
   and no framing says where one octet payload ends.

## Consequences

- A Go generated method takes an `io.Reader` and returns the octets with the
  status and content type; a TypeScript one takes
  `Uint8Array | ArrayBuffer | ReadableStream` and returns the same three facts.
- `go/framework/openapi/binary_integration_test.go` and the four sample cells
  (Go→Go, TS→TS, Go→TS, TS→Go) prove the chain on a real socket.
