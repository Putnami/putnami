# ADR 0010 — A provider-owned WebSocket wire admits on the upgrade, and the handler owns its vocabulary

- **Status**: accepted
- **Scope**: `go.putnami.dev/api` (`go/framework/api`), `go.putnami.dev/http` (`go/framework/http`), `go.putnami.dev/client` (`go/framework/client`)

## Context

[clientcontract ADR 0010](../../../../../protocols/clientcontract/doc/adr/0010-a-provider-owned-websocket-wire-is-declared-not-inferred.md)
owns the language-neutral rules of `wire: "provider"`: the declaration, the
admission on the upgrade request, and the socket budgets. This record states
how Go implements them. [ADR 0004](0004-a-negotiated-websocket-admits-in-band-before-the-security-chain.md)
still governs the first-party conversation, unchanged.

## Decision

**Declaration.** A byte stream is `Body(api.ByteStream())` and
`Returns(api.ByteStream())`, handled by `api.ByteTunnel`. Its
`*api.ByteStreamContext` is an `io.ReadWriter`: `Read` returns `io.EOF` on a
normal client close and the cause otherwise; `Write` splits octets under the
declared frame bound. A provider-owned subprotocol is `.Subprotocol(token)` on
a bidirectional `StreamOf` endpoint handled by `api.BidiStream`; each `Send` is
one JSON text message. `Handle`, `HandleRaw` and `Document` refuse at
registration every declaration the contract refuses, plus a handler of the
other kind.

**Admission.** `http.StreamHandler.AdmitOnUpgrade` marks a wire whose upgrade
request is its admission. The route runs `Before` (security, middleware,
params and query validation) on that request. Such a route has no raw
transport stream and no SSE.

**Close codes.** `1000` when the handler returns, `1008` for a handler error
with a 4xx status, `1001` for 503 and on server drain, `1011` otherwise, and
`1003` for a message of the wrong kind. A normal client close ends only the
client's direction.

**Client.** `client.OpenByteStream` returns a `*client.ByteStream`
(`io.ReadWriteCloser`). `client.OpenFrameStream[TIn, TOut]` returns a
`*client.FrameStream` with `Send`, `Recv` (`io.EOF` on a normal close),
`Close`, `Done` and `Err`. Both drive the same `StreamSession` as every
stream. The generated method returns one of these and names no frame or
socket.

## Consequences

- A provider-owned route serves its wire with or without a published client
  contract: the wire is a fact of the route.

## Rejected alternatives

- **A separate `api.ByteStream` handler builder.** It would be the only stream
  declared outside `Body` and `Returns`.
- **Run the chain in the driver after the upgrade.** A refusal could then only
  be a close code, which carries no declared error.
