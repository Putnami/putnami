# ADR 0005 — A provider-owned WebSocket wire admits on the upgrade request

- **Status**: accepted
- **Scope**: `@putnami/application` (`typescript/framework/application`), `@putnami/client` (`typescript/framework/client`)

[clientcontract ADR 0010](../../../../../protocols/clientcontract/doc/adr/0010-a-provider-owned-websocket-wire-is-declared-not-inferred.md)
owns the language-neutral rules of `wire: "provider"`. The Go implementation is
[go/framework/api ADR 0010](../../../../../go/framework/api/doc/adr/0010-a-provider-owned-wire-admits-on-the-upgrade.md).
This record states what is TypeScript-specific.

## Decision

**Declaration.** `.body(ByteStream()).returns(ByteStream())` declares a byte
stream; the handler reads `Uint8Array` chunks from `ctx.messages()` and writes
with `ctx.send()`. `.subprotocol(token)` on a bidirectional `Stream()` endpoint
declares typed frames: `ctx.messages()` yields validated values and
`ctx.send()` writes one JSON text message each. The handler ends the stream by
returning. The builder throws at registration on every declaration the
contract refuses.

**Admission.** The upgrade takes the SSE path: middleware and params and query
validation run on the upgrade request and refuse with an HTTP response before
any socket exists.

**Close codes.** `1000` when the handler returns, the code the handler's error
status maps to, `1003` for a message of the wrong kind, `1007` for a frame that
is not a valid value of the declared type, `1008` on inbound queue overflow,
`1009` past the frame bound, and `1001` on shutdown.

**Client.** `serviceByteStream` resolves to a `ByteStream` (a `ReadableStream`
and a `WritableStream` of `Uint8Array`, `closed`, `close()`) once the upgrade
is accepted. `serviceFrameStream` returns a `FrameStream` observer with
`send()` and `close()`. A browser `WebSocket` cannot carry headers, so there
the opening fails with `ClientTransportUnavailableError` instead of dialing
anonymously.

## Consequences

- The platform `WebSocket` hides the status of a refused upgrade, so the
  client reports `ClientResponseContractError` (`provider refused the
  websocket upgrade`) without a status. The Go client decodes the declared
  error. Both fail closed.
