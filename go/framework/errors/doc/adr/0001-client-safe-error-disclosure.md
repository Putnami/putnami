# ADR 0001 — Error meaning is inspectable but client disclosure is category-gated

- **Status**: accepted
- **Scope**: `go.putnami.dev/errors` (`go/framework/errors`)

## Context

Errors need a stable machine meaning for routing, retry, logging, and metrics,
with ordinary Go wrapping. The same error can reach an HTTP response or JSON.
Messages, stacks, causes, and attributes can hold query text, paths,
credentials, or infrastructure names.

## Decision

An `Error` carries a code, message, optional cause, attributes, category,
retryability, source, creation time, and an origin stack. `New`, `Newf`, `Bug`,
and `Bugf` create an origin and capture a stack; `Wrap` and `Wrapf` add meaning
without another stack. Inspection walks single- and multi-cause trees. Hooks
observe origin creation only.

Only the `user` and `security` categories are client-safe: `MarshalJSON` and
`WriteHTTPError` expose their message and allowed details. Every other
category, and a missing one, gets the generic internal message and no
attributes. Stack and cause are never serialized. Server-side loggers still
see the full chain.

## Rejected alternatives

- **Expose every message and attribute.** Internal errors carry details unsafe
  for clients.
- **HTTP status as identity.** Many codes share one status.
- **Stack at every wrap.** It hides the origin and inflates logs.
- **Depend on telemetry.** It creates cycles in every framework module.

## Consequences

- Authors classify an error before its message can reach a client.
- A new client-safe category changes disclosure policy and updates the JSON and
  HTTP paths together.
- Error JSON is safe local serialization, not a cross-service wire protocol.
