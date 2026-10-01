# ADR 0006 — An operation declares how it travels, and whether its stream can be continued

- **Status**: accepted
- **Scope**: `go.putnami.dev/api` (`go/framework/api`), `go.putnami.dev/openapi`
  (`go/framework/openapi`), `@putnami/application`
  (`typescript/framework/application`)

## Context

`x-putnami-client` gives every operation an ordered transport list and, on a
WebSocket transport, a `resume` flag. The projection derives the list from the
route's shape (SSE then WebSocket for a server stream, WebSocket alone for a
duplex). A provider needs to reorder it, and needs to state the provider half
of `resilience.stream.reconnect`.

## Decision

**An operation declares its wire preference order, bounded by what its server
serves.** `api.ClientOperationOptions.Transports` in Go and
`.client({ transports })` in TypeScript reorder and narrow the derived list.
They cannot add a transport. A transport named twice, or one the provider
cannot carry for the route's shape, fails projection naming the route.

**An operation declares whether its WebSocket server stream can be
continued.** `ClientOperationOptions.Resume` in Go and
`.client({ resume: true })` in TypeScript set `websocket.resume`. It is legal
only on a **server** stream declared **safe** on the first-party WebSocket
wire; every other shape is refused at projection. The SSE mechanism,
`ClientOperationOptions.SSEContinuation`, is owned by
[clientcontract ADR 0013](../../../../../protocols/clientcontract/doc/adr/0013-an-sse-stream-declares-how-it-continues-and-a-negotiated-stream-ends-explicitly.md);
an effective reconnect needs `websocket.resume` or `sse.continuation`.

**A resume grant is single-use, rotating, bound and in band.** The provider
mints one grant per admission and carries it in `ready`. The grant names the
operation and the client identity, records the highest sequence put on the
wire, and is spent by its one redemption. Every `ready` rotates it. It has a
time to live, a per-stream continuation budget, and a per-endpoint capacity
that drops the oldest first. Grants live in memory only: a restarted provider
cannot prove a position is gap-free, so it refuses the continuation.

**A continuation is an admission.** The grant is redeemed before the
endpoint's security chain runs, so a worthless token costs no credential
check. The chain then runs in full on the request rebuilt from the new `init`.
A credential the session can no longer prove ends the stream with a typed
terminal.

**The handler is told where to continue.** `api.StreamResumeFrom(ctx)` in Go
and `ctx.resumeFrom` in TypeScript carry the sequence to continue after. A
handler that ignores it replays values the consumer already read, which is why
the declaration, not the framework, makes an operation resumable.

## Consequences

- A provider that changes its declared order moves every consumer at the next
  call, with no consumer edit.

## Rejected alternatives

- **Let the consumer configure a transport.** It puts a wire name in consumer
  code and stops the provider retiring one.
- **Derive resume from the stream shape.** Every safe server stream would
  advertise a capability its handler may not honour.
- **Sign a stateless resume token.** A signature proves issue, not that the
  token is unspent; a captured token would replay for its lifetime.
