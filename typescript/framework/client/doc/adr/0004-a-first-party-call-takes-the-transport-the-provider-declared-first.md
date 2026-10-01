# ADR 0004 — A first-party call takes the transport the provider declared first

- **Status**: accepted
- **Scope**: `@putnami/client` (`typescript/framework/client`)

## Context

`x-putnami-client` gives every operation an ordered list of the transports the
provider serves (`connect:proto`, `connect:json`, `rest-json` for a unary
operation behind `grpc()`; `sse`, `websocket` for a server stream). A
per-client transport setting makes the consumer arbitrate a provider decision,
and cannot express a contract whose operations differ.

## Decision

For an operation with a first-party contract, the transport is the first
declared entry this runtime can carry. Selection happens per request at the
end of the interceptor chain, so credentials, deadline, retry, circuit and
telemetry are the same on every wire.

- **Unary:** `connect` with a codec this runtime has (`proto` needs the
  published descriptor), otherwise `rest-json`.
- **Server stream:** the first available of `connect`, `sse` or `websocket`,
  all under `StreamSession` ([ADR 0003](0003-one-stream-session-owns-every-transport-lifecycle.md)).
- **Client and bidirectional streams:** WebSocket, because Connect over
  HTTP/1.1 cannot carry them.

The provider's protobuf descriptor travels with the generated client, so the
binary codec reads field numbers, presence and `json_name` from it. The
transport projects enums between protobuf numbers and JSON strings from the
declared schema.

`ClientConfig.transport` applies only to a client with no contract; it never
overrides the provider.

A unary call never falls back to another wire. A server stream falls back only
before admission, on the answer that a wire is not served, for a
replayable operation
([go/framework/client ADR 0006](../../../../../go/framework/client/doc/adr/0006-a-declared-fallback-happens-before-admission-and-a-continuation-never-repeats-a-value.md)).

## Consequences

- Installing or removing `grpc()` on the provider changes the wire for every
  consumer, with no consumer change or regeneration.

## Rejected alternatives

- **Keep the per-client transport.** Consumer-owned scaffolding that cannot
  express per-operation differences.
- **Try Connect, fall back to REST on any failure.** A down provider looks like
  one without Connect, and an unsafe operation could be replayed.
- **Let the generated method name its transport.** Every added transport would
  force regeneration.
