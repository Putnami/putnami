# Events Protocol

<!-- protocol-version: unversioned -->
<!-- design-stability opt-out: this package has a fixture corpus but no pinned
     `ProtocolVersion` conformance anchor. Its wire version is the
     `putnami.events.v1` string token carried by documents, which no test pins
     against a Go constant. Remove this marker once a `const ProtocolVersion` is
     declared and pinned in a `TestConformance_ProtocolVersion`. -->

Canonical Putnami Event Server protocol: event envelope, stream frames,
discovery, endpoint profiles, and handler semantics.

Putnami Event Plane is the managed Event Server offering: the simplest managed
default when an application wants SSE, WebSocket, HTTP publish, gRPC, replay,
routing, and provider abstraction without operating Redis, Pub/Sub, or Postgres
plumbing directly. The protocol remains open so self-hosted Event Servers and
framework implementations can stay compatible.

This module is the cross-language source of truth for:

- framework event envelopes
- WebSocket/mobile stream frames
- gRPC Event Server service definition
- event-server discovery and endpoint profiles
- delivery profiles (`pull`, `stream`, `push`) and the push receiver contract
- the managed-workload publish profile and its versioned shared corpus
- handler option vocabulary
- broker mapping expectations
- cloud Event Server implementation expectations
- conformance fixtures and runner manifest for compatible implementations

Go consumers import:

```go
import events "go.putnami.dev/protocol/events"
```

TypeScript consumers should use `@putnami/events/protocol`.

Managed publish conformance runners can load the embedded v1 corpus without a
repository-relative path:

```go
fixtures := events.ManagedPublishV1Fixtures()
request, _ := fs.ReadFile(fixtures, "valid/minimal.json")
```

## Source of truth

The canonical contract is the JSON Schemas in `schemas/` and the fixture corpus
in `fixtures/`, backed by the Go strict parsers and validators in this package
(`go.putnami.dev/protocol/events`). The TypeScript surface
(`@putnami/events/protocol`) and the Go framework types are kept in step by
running the same fixtures through each language — see `conformance_test.go` and
the `@putnami/events` protocol conformance test.

The stricter managed-workload profile remains on the same
`putnami.events.v1` wire version. Its `eventServer.contractVersion: 1` config
gate, schemas, request fixtures, and outcome fixtures are documented in
[`doc/12-managed-publish.md`](doc/12-managed-publish.md). The corpus is embedded
by this Go module so cloud and framework consumers use the same bytes instead of
maintaining copies.

The gRPC endpoint profile is illustrated by a **non-normative** protobuf file:

```text
proto/putnami/events/v1/event_server.proto
```

It is reference documentation only: no code generation is wired to it, and it
must not be treated as a second source of truth. If the proto and the schemas
disagree, the schemas win. The reasoning, and the alternatives that were
rejected, are in
[`doc/adr/0001-schemas-are-the-source-of-truth.md`](doc/adr/0001-schemas-are-the-source-of-truth.md).

Compatible Event Servers expose discovery at:

```text
GET /.well-known/putnami/events
```

The discovery document advertises protocol version, supported endpoint
profiles (`sse`, `websocket`, `http`, `grpc`), feature flags, concrete
endpoint paths/services, and limits. Client libraries, Putnami Event Plane, and
self-hosted Event Servers should validate this document before connecting.

Event Server implementations should also run the public black-box conformance
suite described by:

```text
conformance/manifest.json
```

The runner targets deployed endpoints, so private server implementations can
stay private while still proving compatibility with the public clients and
protocol contract.

## Producers and consumers

| Role | Who |
| --- | --- |
| Producers | any application publishing through `@putnami/events` or the Go events framework; any Event Server emitting stream frames, discovery documents, or gateway errors; a managed workload publishing through the managed-publish profile |
| Consumers | client libraries over SSE, WebSocket, HTTP and gRPC; push receivers; Putnami Event Plane; self-hosted Event Servers; broker adapters |
| Owner of this contract | this project — it declares the shapes, and neither the framework nor a server implementation may widen them locally |

The protocol is deliberately open: the managed offering and a self-hosted
Event Server are two implementations of the same document set, and neither is
privileged by the contract.

## Schemas and fixtures

- Schemas: [`schemas/`](schemas) — `envelope.json`, `gateway-frame.json`,
  `gateway-capabilities.json`, `gateway-error.json`, `handler-options.json`,
  `push-delivery.json`, `conformance-manifest.json`,
  `managed-publish-frame-v1.json`, `managed-publish-outcome-v1.json`.
- Fixtures: [`fixtures/`](fixtures) — envelope/gateway (6 valid, 9 invalid),
  handler options (2 valid, 8 invalid), push delivery (2 valid, 3 invalid),
  conformance manifests (1 valid, 2 invalid), and the managed-publish v1 corpus
  (4 valid requests, 25 invalid requests, 8 outcome vectors).
- Non-normative reference: [`proto/`](proto) — see the source-of-truth section
  above.
- Endpoint-level suite: [`conformance/manifest.json`](conformance/manifest.json).

## Versioning and compatibility

The wire version is a **string token**, not an integer: every document that
names the protocol carries `putnami.events.v1` (the `Protocol` constant), and a
document naming anything else is rejected with the `invalid-protocol`
diagnostic. There is no `ProtocolVersion` integer in this package, which is why
the design-stability marker at the top of this file is still present.

What is additive, and stays on `putnami.events.v1`:

- a new optional member on an existing shape;
- a new endpoint profile advertised through discovery, since a client selects
  the profiles it understands;
- a new fixture, valid or invalid.

What is breaking, and needs a new protocol token:

- removing or renaming a member, or making an optional member required;
- adding or repurposing a value in a closed vocabulary — frame types, endpoint
  profiles, delivery profiles, acknowledgement/overflow/distribution modes —
  because a client switches on those values and cannot distinguish an unknown
  one from a malformed one;
- changing the meaning of a reserved managed-publish attribute prefix.

The stricter managed-workload profile is **not** a second wire version: it
stays on `putnami.events.v1` and is gated separately by
`eventServer.contractVersion: 1` in config, so a stricter server can be required
without splitting the protocol.

**Cross-implementation parity is real here and is tested.** The Go strict
parsers in this package and the TypeScript surface `@putnami/events/protocol`
validate the *same* fixture files
(`typescript/framework/events/test/protocol-conformance.test.ts` reads
`protocols/events/fixtures`), so a shape that drifts in one language fails in
the other. Server implementations prove endpoint-level compatibility separately
through `conformance/manifest.json`.

## Support status

- **Subject**: `go.putnami.dev/protocol/events`, kind `protocol`.
- **Status**: `preview`, recorded in the workspace-root
  [`putnami.support.json`](../../putnami.support.json) — the only reviewed
  authority for support status (contract:
  [`protocols/support/README.md`](../support/README.md)).
- **Owner**: this project (`protocols/events`). It owns the schemas, the
  fixture corpus, and the Go strict parsers; framework packages and Event Server
  implementations are consumers of that authority, not co-owners of it.
- **Evidence for `preview` rather than `stable`**: the corpus is large and
  genuinely cross-language — 15 envelope/gateway fixtures, 10 handler-option
  fixtures, 5 push fixtures, 3 conformance-manifest fixtures and 37
  managed-publish vectors, all validated by both the Go parsers
  (`conformance_test.go`, `managed_publish_test.go`, `push_test.go`,
  `invalid_fixture_coverage_test.go`) and the TypeScript protocol surface — but
  the module still has **no pinned version anchor**: no `const ProtocolVersion`,
  and no test asserting the `putnami.events.v1` token against a schema `const`.
  A version token that only prose defends is not a compatibility commitment, so
  `stable` is not claimed. Declaring and pinning that anchor is the concrete work
  that would earn it.
- **`default` and `parity`**: no claim is recorded on either axis. Omission is
  not a denial — see the support vocabulary.

## Specs and durable decisions

There is deliberately **no user-facing feature or spec for this module**. A
feature declares a product outcome a user obtains; this module declares the
bytes several products exchange. Minting `events/protocol` as a feature would
create a product promise with no user on the other end of it, and would put a
second authority beside the schemas.

The user-facing surface that owns the outcome is the **events capability of a
Putnami application** — `@putnami/events` and the Go events framework, the
managed Event Plane offering, and the client libraries. A spec belongs with
whichever of those a change is actually for; this module records only the wire
decisions behind it.

Durable decisions recorded here:

- [`doc/adr/0001-schemas-are-the-source-of-truth.md`](doc/adr/0001-schemas-are-the-source-of-truth.md)
  — the JSON Schemas and the fixture corpus are canonical; the protobuf file is
  non-normative reference documentation for the gRPC endpoint profile, and the
  schemas win any disagreement.

The prose specification of each shape lives in [`doc/`](doc), from the envelope
(`01-envelope.md`) through push delivery and the managed-publish profile
(`11`, `12`).
