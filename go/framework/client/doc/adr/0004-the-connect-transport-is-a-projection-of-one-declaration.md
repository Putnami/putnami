# ADR 0004 — The Connect transport is a projection, not a second client

- **Status**: accepted
- **Scope**: `go.putnami.dev/client` (`go/framework/client`)

## Context

REST puts a path parameter in the URL; Connect puts it in a request message.
Everything else (credential, deadline, retry, breaker, trace headers, response
validation, typed error) is one declaration. A second entry point, resilience
chain and error decoder for Connect would diverge from REST.

## Decision

**A generated method describes the call once.** It hands the runtime an
`OperationCall`: the REST request plus the path parameters it was built from.
The runtime picks the wire. Unary methods call `client.CallOperation`; server
streams call `client.OpenOperationServerStream`.

**The declared order is the dispatch order.** The client takes the first
transport in `operation.transports` it can drive. A failing unary transport
never becomes another one; stream fallback is
[ADR 0006](0006-a-declared-fallback-happens-before-admission-and-a-continuation-never-repeats-a-value.md).

**The Connect path rides the existing chain** over the same bound client, so
credentials, deadline, retries, breaker and trace propagation are shared.
What leaves the transport is the first-party shape: the declared success
status with the schema's JSON, or the first-party error envelope rebuilt from
the Connect error document. `ServiceCallInfo.Protocol` reports the dispatched
transport.

**One error encoding for both providers.** The Connect `code` is one of the
sixteen categories. The framework code, exact HTTP status and declared details
come from a `putnami.client.v1.FrameworkError` detail
([@putnami/application ADR 0004](../../../../../typescript/framework/application/doc/adr/0004-connect-carries-the-first-party-error-as-a-typed-detail.md)).
Without it (a third-party service), the code falls back to the Connect code
name and the status to the canonical mapping, and no declared error is
selected. A body that is not a Connect error document falls back to status
inference and yields `client.remote` with the real status.

**The proto codec reads the published descriptor**: field numbers, wire
kinds, presence, map shapes and enum numbers. No stub is generated. The codec
is the shared `go.putnami.dev/protocol/clientcontract/connect` package that
`go/framework/grpc` also uses.

**A Connect server stream drives `StreamSession`**, with admission at the
accepted response headers.

**Refused rather than degraded:**

- a `proto` transport with no published descriptor;
- a repeated query parameter or a non-object body when Connect is dispatched;
- a nullable member over `connect+proto`, at generation: proto3 has one
  absence, and a Go caller reading `Optional.IsNull()` would see the two
  encodings disagree. `connect+json` and `rest-json` carry the null.
