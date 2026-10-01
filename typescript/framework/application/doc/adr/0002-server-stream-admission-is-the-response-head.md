# ADR 0002 — Server-stream admission is the response head

- **Status**: accepted
- **Scope**: `@putnami/application` (`typescript/framework/application`)

## Context

An SSE response commits its status and headers before the first body byte.
After the head, a refusal can only be a terminal event on a 200. The client
contract defines SSE admission as a 2xx with the declared media type, and
consumers derive retry, credential invalidation and breaker accounting from it
([clientcontract ADR 0005](../../../../../protocols/clientcontract/doc/adr/0005-stream-sessions-have-five-phases-and-four-budgets.md)).
A provider that answers 200 and then writes an error event claims it accepted
an operation it refused.

A stream that writes an oversize frame, or nothing for longer than a consumer's
idle budget, is indistinguishable from a broken or dead one.

## Decision

Everything that can refuse a server-stream operation runs before the response
head, and everything written after it is bounded.

Admission is refused with an HTTP status, never a 200 followed by an event:

- A request with a body gets 400. A server stream is a GET with no declared
  body, so accepting one would silently discard it.
- Params and query validation run before the stream is created.
- Authentication and authorization are endpoint middleware (`.secure(...)`),
  which the api plugin wraps around the SSE handler. A first-party contract
  cannot represent a custom verifier or guard, so an application-level
  middleware establishes identity once, before every route, and the endpoint
  declares only what it requires.

Past the head, three bounds apply:

- `maxBufferedMessages` bounds the queue and reserves one slot for the terminal
  event.
- `maxFrameBytes` bounds a single frame. The consumer contract requires the
  other end to reject a larger frame, so failing at the producer names the
  producer.
- `heartbeatMs` (default 15 s) writes a data-less comment frame that only the
  consumer's idle budget observes. A heartbeat is skipped, not queued, while the
  consumer is behind, so it never takes the terminal slot. The timer is unref-ed.

The terminal error event carries the operation's stable wire code (`not_found`),
not the PascalCase `.mayThrow()` identifier (`NotFound`).

## Rejected alternatives

- **Authenticate inside the stream handler.** It produces a 200 followed by an
  error event.
- **Leave the frame bound to the consumer.** The consumer rejects it as a
  violation by a provider that could have known.
- **Heartbeat by re-sending the last message.** A consumer cannot tell liveness
  from a duplicate update; a comment is invisible to the message stream.
- **Keep the runtime error code on the wire.** A consumer generated from the
  contract cannot narrow it.

## Consequences

- A stream endpoint that needs identity depends on an installed
  application-level resolver. Without one, `.secure(...)` answers 401 for
  everyone: closed, not open.
- A provider that produces a frame past `maxFrameBytes` fails the stream.
