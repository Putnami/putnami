# ADR 0008 — A call delivers its success body through a caller-owned sink

- **Status**: accepted
- **Scope**: generated Go and TypeScript service clients

## Context

Some answers are verified as canonical bytes (a release-set resolve body whose
digest derives from its bytes, a signed namespace snapshot). A generated call
returns the decoded value, and re-marshalling it produces other bytes. Runtime
entry points such as `CallOperationBytes` are unreachable, because generated
clients keep their operation descriptors unexported.

## Decision

This record owns the shared rule; the TypeScript surface is
[`@putnami/client` ADR 0007](../../../../../typescript/framework/client/doc/adr/0007-a-call-delivers-its-success-body-through-a-caller-owned-sink.md).

The caller passes a sink with the call: `client.WithSuccessBody(ctx, &sink)`
in Go, the generated `successBody` option in TypeScript. The generated method
runs unchanged; on success the sink holds the declared JSON success body
exactly as the provider sent it. No bytes-returning sibling method is
generated.

- Bytes are delivered only after every check the call applies (security,
  resilience, status, media type, schema projection, typed decode).
- A cached answer delivers the bytes stored with it; the sink and the cache
  hold separate copies. A failed call leaves the sink empty.
- Only `rest-json` and Connect `json` deliver. Connect `proto` is refused
  before dispatch with the transport named. A void operation, a raw octet
  payload and a stream refuse the sink before anything is sent.
- A sink belongs to one call, and the selection is consumed by that call: a
  nested generated call in a credential provider or interceptor neither fills
  it nor is refused because of it.

## Consequences

- The runtime keeps one decode path, and the delivered bytes are the ones it
  decoded.
- The TypeScript generator forwards the option on every method shape, so the
  runtime decides where it is refused.
