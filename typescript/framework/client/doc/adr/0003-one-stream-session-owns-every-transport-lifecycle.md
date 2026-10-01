# ADR 0003 — One stream session owns the lifecycle of every transport

- **Status**: accepted
- **Scope**: `@putnami/client` (`typescript/framework/client`)

## Context

[clientcontract ADR 0005](../../../../../protocols/clientcontract/doc/adr/0005-stream-sessions-have-five-phases-and-four-budgets.md)
owns the language-neutral session contract, and
[go/framework/client ADR 0002](../../../../../go/framework/client/doc/adr/0002-stream-sessions-have-five-phases-and-four-budgets.md)
states the breaker rules both runtimes apply. This record states what is
TypeScript-specific.

## Decision

`StreamSession` owns the lifecycle, the single terminal and measurement, the
four budgets, the credential re-check, the one-shot credential invalidation,
and every breaker write. A transport (SSE, Connect, first-party and
provider-owned WebSocket) turns bytes into values and nothing else.

- **The handshake budget is a session timer, not a bound on the attempt's
  abort signal.** The same signal reads the body for the life of an admitted
  stream, so a handshake bound on it would cut a healthy stream.
- **`maxBufferedMessages` overflow ends the stream** with the declared queue
  error. Buffering past the bound makes it a suggestion; dropping silently is
  what the contract forbids.
- **An SSE stream has a circuit and a call measurement**, so a provider that
  refuses SSE handshakes opens the same circuit as its unary siblings.
