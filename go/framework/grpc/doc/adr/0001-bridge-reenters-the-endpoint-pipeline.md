# ADR 0001 — The Connect bridge re-enters the endpoint pipeline

- **Status**: accepted
- **Scope**: `go.putnami.dev/grpc` (`go/framework/grpc`)

## Context

A service declares its operations once as api endpoints. A second handler per
operation for Connect callers would duplicate validation, security, and error
shape, and the copies would drift.

The mapping is not mechanical. An endpoint takes path parameters, query
parameters, and a body; flattening them into one message collides whenever a
name repeats, as in `PUT /users/{id}` with a body field `id`. JSON numbers
decoded as `float64` also corrupt identifiers above 2^53. The published
protobuf descriptor already carries every field number, wire kind, presence
flag, map shape, and enum number a binary encoding needs.

## Decision

**The bridge wraps the endpoint's own built handler.** For each bound endpoint
it registers `POST /<package>.<Service>/<RPCName>`, decodes the request,
projects it onto the endpoint request, and calls the handler unchanged.
Validation, security, middleware, and error responses match REST by
construction. The request is the namespaced envelope
`{ "params": {…}, "query": {…}, "body": {…} }`; a flat object is refused. JSON
envelopes decode with `UseNumber`, so numbers keep their decimal text.

**The published descriptor decides the URLs.** When the api plugin publishes a
descriptor, each URL is the method identity from
[`Document.RouteMethods()`](../../../proto/doc/adr/0002-a-descriptor-states-the-wire-shape-the-schema-states.md),
and a route the descriptor leaves out gets no Connect URL. `Start` fails when
a declared method has no mounted URL. Only a provider with no descriptor
computes names with the shared `clientcontract.RPCName`. Document-only
endpoints, external-authority routes, and client or bidirectional streams get
no URL: Connect carries at most one request message per call. A declared
server stream is served.

**The descriptor is the codec.** With a descriptor, the bridge also serves
`application/proto` and `application/connect+proto` by walking it, with no
generated stub. Without one, it serves JSON only. The contract advertises
exactly the encodings the mounted bridge answers. The JSON side is the
published schema's JSON, not proto3 canonical JSON: a 64-bit integer is a
number, an enum is its published member, a Duration is int64 nanoseconds. One
value is therefore identical on `rest-json`, `connect+json`, and
`connect+proto`.

**Errors are the Connect document** `{code, message, details}`, where `code` is
one of the sixteen specification codes. The first-party facts travel as the
typed detail `putnami.client.v1.FrameworkError` (`code = 1` framework code,
`http_status = 2` the REST status, `details_json = 3` the declared details as
verbatim JSON), the same message the
[TypeScript provider publishes](../../../../../typescript/framework/application/doc/adr/0004-connect-carries-the-first-party-error-as-a-typed-detail.md).
Verbatim JSON keeps 64-bit numbers intact, which `google.protobuf.Struct`
would not. A third-party client reads the code and ignores the detail.

**Two deliberate deviations from the specification:**

1. A successful unary response is HTTP 200 even when the endpoint declares 201
   or 204; the operation contract carries the declared status and a
   first-party client restores it.
2. A stream rejected before its response headers keeps its real HTTP status,
   so a client can tell "refused" from "accepted, then failed" for admission,
   retry, and circuit rules. After the headers, every error travels in
   `EndStreamResponse`.

**Bounds.** `ApiBridgeConfig.MaxMessageBytes` bounds every decoded message
before allocation, compressed and decompressed. `Connect-Timeout-Ms` bounds the
endpoint context; a malformed value is refused, never ignored. A stream clears
the server-wide write deadline and re-arms a per-write one
(`WithStreamWriteTimeout`, 30 s default).

## Rejected alternatives

- **A second handler per operation.** The two implementations drift silently.
- **Flatten params, query, and body.** It breaks on the most common update
  shape.
- **Default number decoding.** `float64` corrupts identifiers above 2^53.
- **Bridge document-only endpoints.** Their URL could only answer 500.
- **Compute RPC names apart from the descriptor.** Client and served URL would
  disagree, and only a live call would show it.
- **Canonical proto3 JSON.** Values would differ between REST and Connect.
- **User-defined Connect codes.** The specification defines none.

## Consequences

- A change to how endpoints build handlers changes bridged behavior too.
- A provider that mounts the proto plugin advertises `[json, proto]`.
- `google.rpc.BadRequest` is not emitted: validation failures leave the
  pipeline as the first-party envelope.
- The Connect wire code lives in `go.putnami.dev/protocol/clientcontract/connect`,
  shared by this bridge and `go.putnami.dev/client`. Its tests read the shared
  conformance corpus
  `typescript/framework/application/test/grpc/connect-conformance/corpus.json`,
  so the Go and TypeScript implementations answer to one document.
- Changing `clientcontract.RPCName` is a breaking change for every provider
  without a descriptor and needs a documented migration.
