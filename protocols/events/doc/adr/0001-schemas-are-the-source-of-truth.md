# ADR 0001 — The JSON Schemas are the source of truth; the proto is a reference

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/events` (`protocols/events`)

## Context

Four endpoint profiles (SSE, WebSocket, HTTP, gRPC) share one protocol
identifier and one field vocabulary. Two artifacts describe it: the JSON
Schemas in [`schemas/`](../../schemas) with the corpus in
[`fixtures/`](../../fixtures), backed by the Go strict parsers and re-validated
by `@putnami/events/protocol`; and
[`event_server.proto`](../../proto/putnami/events/v1/event_server.proto) for the
gRPC profile. They overlap almost completely, so they will disagree, and a
generator wired to the losing one would ship the disagreement.

## Decision

The schemas and the fixture corpus are canonical. The proto is non-normative
reference documentation for the gRPC profile. On disagreement, the schemas win.

The demotion is structural:

- the proto carries a `NON-NORMATIVE` banner naming the canonical artifacts;
- it declares no `option go_package`;
- no build step reads it;
- `TestConformance_GRPCProtoIsNonNormativeReference` asserts all three, and
  that the service and RPC surface is still present.

## Rejected alternatives

- **Make the proto canonical with codegen.** Privileges one of four profiles,
  puts a protobuf toolchain in front of SSE and HTTP consumers, and orphans the
  corpus.
- **Delete the proto.** It is the only precise statement of the gRPC service
  and method names, and discovery advertises that service name.
- **Both normative, kept in sync.** Nothing enforces it.
- **Generate the proto from the schemas.** A generator and drift check to keep
  one documentation file honest.

## Consequences

- A gRPC implementer hand-writes the service from the proto and validates
  payloads against the schemas and corpus.
- A schema change is the wire change; the proto is updated by hand afterwards.
  The guard pins the demotion, not synchronization, so a stale proto is a
  documentation bug.
