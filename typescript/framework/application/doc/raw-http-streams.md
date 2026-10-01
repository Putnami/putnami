# Raw HTTP streams

Use `BinaryStream({ maxBytes })` when the HTTP body itself is an opaque stream and its
concrete media type is known at runtime:

```ts
endpoint()
  .body(BinaryStream({ maxBytes: 64 * 1024 * 1024 }))
  .returns(BinaryStream({ maxBytes: 64 * 1024 * 1024 }))
  .handle(async (ctx) => new HttpResponse(await ctx.body(), {
    headers: { 'Content-Type': ctx.headers.get('Content-Type')! },
  }));
```

The handler receives a bounded `ReadableStream<Uint8Array>` without a whole-body
read. The incoming and outgoing content types must be concrete
type/subtype values; wildcard, missing and malformed values are refused.
Parameters and octets are preserved, including JSON-labelled non-JSON bytes.

OpenAPI publishes `*/*` with `type: string`, `format: binary` and
`x-putnami-streamed: true` and the positive `x-putnami-max-bytes`. The provider
answers 413 when the upload crosses it. Existing `Binary({ maxBytes, mediaType })`
keeps its bounded buffering behavior. Both shapes remain unary HTTP; neither
introduces WebSocket messages or Connect envelopes.

Consume the upload before returning a response when an overflow must produce
status 413. A direct streaming echo, as above, may discover the overflow after
response headers have been sent; that failure terminates the response body
because HTTP cannot replace an already-sent status.

A streamed response keeps the request scope alive until EOF, cancellation or
failure. Its reader owns resource cleanup; consumers must drain or cancel it.
Operation response caches are refused, and generated clients never replay a
streamed upload after a retryable error or credential refresh.

See [ADR 0011](../../../../go/framework/api/doc/adr/0011-streamed-octets-carry-their-own-media-type.md).
