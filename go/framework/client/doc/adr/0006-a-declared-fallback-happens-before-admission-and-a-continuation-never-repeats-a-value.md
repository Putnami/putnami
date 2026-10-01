# ADR 0006 — A declared fallback happens before admission, and a continuation never repeats a value

- **Status**: accepted
- **Scope**: `go.putnami.dev/client` (`go/framework/client`), `@putnami/client`
  (`typescript/framework/client`)

## Context

`x-putnami-client` publishes an ordered transport list per operation, and a
provider declares the order and `websocket.resume`
([go/framework/api ADR 0006](../../../api/doc/adr/0006-an-operation-declares-how-it-travels-and-whether-it-can-be-continued.md)).
A deployment may serve only one of two declared wires, and a socket may break
mid-stream on an operation declared resumable. Both mean opening a connection
a second time, which is legal only when it cannot deliver or send anything
twice.

## Decision

**A fallback is a runtime act, before admission, on one failure class.**

- The runtime walks the declared transports in order and opens the first it
  can carry. It opens the next only when the provider answered that the wire
  is not served at this path: HTTP 404, 405 or 426, gRPC status 12
  `UNIMPLEMENTED`, or a completed RFC 6455 handshake that did not select
  `putnami.service.v1`. Every other answer is a fact about the call.
- A fallback is legal only for an operation declared `safe` or `idempotent`.
  Anything else gets one attempt on its first declared transport.
- A client or bidirectional stream never falls back: the caller's messages
  would travel twice.
- After admission there is no fallback.

The method signature is the same whatever the carrier, so this belongs to the
runtime: a server stream has one entrypoint (`client.OpenOperationServerStream`
in Go, `serviceStream` in TypeScript) for every declared order. A unary call
never falls back.

**A continuation is one session, one measurement, one breaker verdict.**

- It happens only when the shape is a server stream declared `safe`, the
  operation declares an effective `resilience.stream.reconnect`, and the
  transport declares `websocket.resume` or `sse.continuation`. The SSE
  mechanism (cursor versus best-effort, the negotiated terminal, the reopened
  query) is owned by
  [clientcontract ADR 0013](../../../../../protocols/clientcontract/doc/adr/0013-an-sse-stream-declares-how-it-continues-and-a-negotiated-stream-ends-explicitly.md).
- Only a transport break (a connection that ended without a terminal while the
  session was live) triggers it. A contract violation, a typed error, a budget
  expiry and a caller withdrawal never do.
- The reopening carries the position of the last message the caller
  completely received; a message decoded but not handed over does not count.
  The runtime alone sets that position. A WebSocket reopening sends the token
  the previous `ready` issued in the new `init`.
- Credentials are re-resolved before the reopening dials; a failed
  re-resolution ends the session.
- A provider that answers a WebSocket continuation with a fresh stream is
  refused: it would repeat values already read.
- Continuations are capped at five per session (`maxStreamResumeAttempts`). The
  cap is a bound; nothing continues unless declared.
- Every connection of one stream belongs to the same `StreamSession`.

**A client or bidirectional stream is never continued**, and the emitters
refuse `websocket.resume` on either at generation.

## Consequences

- A provider can move an operation between declared wires, or add one, with
  no consumer edit, and a deployment missing one wire degrades to the next.
- A caller of a resumable server stream reads one sequence across a broken
  connection, with no gap or duplicate, or gets a typed terminal.

## Rejected alternatives

- **Fall back on any pre-admission failure, timeout or 5xx.** A caller could
  not tell a down provider from one that does not serve a wire, and a slow
  provider would be asked twice.
- **Retry the same transport before falling back.** `resilience.retry` already
  runs inside one attempt; a second policy would multiply them.
- **Let the caller pass a resume position.** The position and token are
  provider material; a composed one could claim a position never reached.
- **Resume without rotating the token.** An observed token would open the
  stream for its whole lifetime.
