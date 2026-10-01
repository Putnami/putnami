# ADR 0005 — A generated stream method states its shape, and the runtime picks the carrier

- **Status**: accepted
- **Scope**: `go.putnami.dev/api` (`go/framework/api`), `@putnami/client` (`typescript/framework/client`)

## Context

A first-party provider publishes, per operation, a stream shape (`server`,
`client`, `bidirectional`) and an ordered list of transports that carry it.
The generated method must not change when the provider changes that order,
and a consuming application must never branch on a transport.

## Decision

**The generated method signature states the shape. The declared transport
order, read by the runtime, states the carrier.**

- A server stream emits one method returning a stream handle, whatever the
  declared order: `client.OpenOperationServerStream` in Go, `serviceStream`
  in TypeScript. The runtime walks the declared transports at call time
  (Connect, SSE, first-party WebSocket) and opens the first it can carry.
  A fallback is a second opening of the same method, never a second method
  ([client ADR 0006](../../../client/doc/adr/0006-a-declared-fallback-happens-before-admission-and-a-continuation-never-repeats-a-value.md)).
- A client stream and a bidirectional stream emit a typed duplex handle:
  `*client.RequestStream[TIn, TOut]` and `*client.BidiStream[TIn, TOut]` in Go
  (`client.OpenClientStream`, `client.OpenBidiStream`), `DuplexStream<Send,
  Message>` in TypeScript. `TIn` is `messages.input`, `TOut` is
  `messages.output`.
- No generated method takes a transport argument.

**What the emitter cannot carry exactly, it refuses by name at generation.**
A WebSocket transport encoded as `proto`, a `resume` declared on a client or
bidirectional stream, a WebSocket transport without the `putnami.service.v1`
subprotocol, and a duplex stream missing a message schema raise
`clientgen_unsupported_semantic` naming the operation. The runtimes keep their
own refusals for hand-written or stale descriptors.

**No wire state reaches emitted code.** A generated method calls `Send`,
`CloseSend`, `Recv`, `Result`, `send`, `end` and nothing else. Frame
sequences, half-close bookkeeping and phase rules stay in the runtime, which
drives `WebSocketConversationV1`.

## Consequences

- Moving an operation between transports never regenerates the method.
- `resilience.stream` is read from the published contract by both ends, so a
  provider that changes a budget moves both sides at the next generation.

## Rejected alternatives

- **Emit one entrypoint per transport and let the application pick.** It puts
  a transport name in consumer code and stops a provider retiring a wire.
- **Prefer WebSocket over the declared order.** It makes the published order
  decorative.
- **Refuse `proto` and `resume` at runtime only.** A client that compiles and
  fails on its first frame is a permissive fallback.
