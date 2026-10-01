# ADR 0003 — The WebSocket client is hand-written and admits in band

- **Status**: accepted
- **Scope**: `go.putnami.dev/client` (`go/framework/client`)

## Context

A provider can declare a first-party `websocket` transport for any stream
shape. `go.putnami.dev/protocol/clientcontract` publishes the frame
vocabulary, the transition table (`NextWebSocketStateV1`) and the conversation
view (`WebSocketConversationV1`)
([clientcontract ADR 0002](../../../../../protocols/clientcontract/doc/adr/0002-websocket-admission-uses-a-first-frame-state-machine.md)).
`StreamSession` owns the lifecycle ([ADR 0002](0002-stream-sessions-have-five-phases-and-four-budgets.md)).
A runtime that restates either drifts.

## Decision

**The RFC 6455 client is written by hand on `net` and `net/http`**, with no
external dependency. It performs the opening handshake, masks every frame it
sends, refuses a masked provider frame, reassembles continuations, answers
pings, and enforces the declared frame and message bounds. The accept digest
is pinned to the RFC 6455 example.

**The runtime states no wire rule and no phase rule.** Every frame goes
through one `WebSocketConversationV1`; every lifecycle fact goes to
`StreamSession`. A scan fails if a `ws_*.go` file names a conversation state or
the transition function.

**Admission travels in the first application frame.** The handshake offers
only the RFC 6455 headers and `putnami.service.v1`. The init frame is
marshalled, re-parsed by the strict parser and accepted by a throwaway
conversation before any byte is sent, so a frame the wire would refuse is a
local fault with the contract's diagnostic code.

**The contract is consulted before runtime limits.** An undeclared resume is
refused with `client_contract.invalid_resilience` before the runtime refuses
an encoding it cannot produce. A `proto` WebSocket transport is refused at
open, before any network byte.

**A bidirectional result value is the last `Recv` value before `io.EOF`**; a
client stream returns it from `Result`. Each stream keeps one delivery channel.

**The heartbeat and the declared handshake budget** are read by this transport
(`resolveWebSocketHeartbeat`, `resolveWebSocketBudgets`) because
`StreamBudgets` has no heartbeat field.

## Consequences

- A real client replays `protocols/clientcontract/fixtures/websocket` against a
  scripted provider. Two scenes whose invalid frame is one this client cannot
  compose (a cancel before init, an init with an unknown secret-shaped member)
  are proved by negative tests instead.
