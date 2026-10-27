# gRPC Services Protocol

`putnami.grpc-services.v1` is the language-neutral, deterministic inventory of
the gRPC and Connect services an application registered. A framework writes it
at build time to `schema/grpc-services.json`; a consumer reads it to know which
services a committed tree serves, without running the workload or parsing its
source.

The inventory is its own protocol, not a field of the HTTP route inventory: a
native gRPC service has no HTTP route, and a Connect service is reached through
a route that `putnami.http-routes.v1` already lists. That decision is recorded
in [`doc/adr/0001-grpc-services-is-its-own-protocol.md`](doc/adr/0001-grpc-services-is-its-own-protocol.md).

The normative artifacts are the JSON Schema
[`schemas/grpc-services.json`](schemas/grpc-services.json) (its `$id` is
`https://putnami.dev/schemas/putnami-grpc-services-v1.json`) and the corpus
under [`fixtures/`](fixtures/). The Go validator is stricter than the schema
where JSON Schema cannot express a rule: a Connect `basePath` must end in the
service's own name, and its mount prefix has no dot segment.

## Producers and consumers

| Role | Implementation |
| --- | --- |
| Producer (Go) | `go.putnami.dev/grpc` — the native `Plugin`, the Connect `GatewayPlugin` and the `ApiBridge` write one union inventory to `schema/grpc-services.json` during describe (`go/framework/grpc/services_inventory.go`). |
| Producer (TypeScript) | None. No TypeScript framework surface serves gRPC. |
| Consumer (Go) | `@putnami/go`, a strict validator that controls promotion (`go/extension/internal/codegen/describe.go`). Before describe commits any schema, it checks the inventory the run wrote with `ParseAndValidateManifest` and refuses bytes that differ from `CanonicalJSON`, so a malformed, other-version, wrong-digest or non-canonical document fails the build and changes no committed file. It commits a valid document verbatim and removes a stale copy when describe succeeds without writing one. It reads no service fact. |
| Consumer (joins) | None in this repository. A consumer joins the inventory with the same build's `schema/http-routes.json`; the producer tests prove that join. |

A present file is evidence: the application composes a gRPC surface, and the
file lists every service it registered. An empty `services` list states that the
surface registers no service. An absent file is no evidence at all; a consumer
must not infer a gRPC surface, or its absence, from the language or `go.mod`.

## Wire shape

```json
{
  "$schema": "https://putnami.dev/schemas/putnami-grpc-services-v1.json",
  "protocol": "putnami.grpc-services.v1",
  "services": [
    {
      "name": "grpc.health.v1.Health",
      "transport": "grpc",
      "methodsKnown": true,
      "methods": [
        { "name": "Check", "clientStreaming": false, "serverStreaming": false },
        { "name": "Watch", "clientStreaming": false, "serverStreaming": true }
      ],
      "provenance": {
        "project": "example/orders",
        "package": "go.putnami.dev/grpc",
        "sourceKind": "grpc-server"
      }
    },
    {
      "name": "pkg.v1.Echo",
      "transport": "connect",
      "basePath": "/rpc/pkg.v1.Echo/",
      "methodsKnown": false,
      "methods": [],
      "provenance": {
        "project": "example/orders",
        "package": "go.putnami.dev/grpc",
        "sourceKind": "connect-gateway"
      }
    }
  ],
  "digest": "sha256:..."
}
```

## Fields and rules

- `name` is the fully qualified service name: one or more identifiers matching
  `[A-Za-z_][A-Za-z0-9_]*`, joined by `.`. A single segment is valid; an empty
  segment is not.
- `transport` is `grpc` (a native gRPC server) or `connect` (Connect over an HTTP
  server).
- `basePath` is the Connect URL prefix of the service. It is required for
  `connect` and forbidden for `grpc`. It is absolute, ends in `/<name>/`, and may
  sit behind a mount prefix of plain segments: no empty segment, no dot segment,
  no percent escape.
- `methodsKnown` states whether `methods` lists every RPC. When it is `false`,
  `methods` is empty: the producer cannot observe the methods and lists none
  rather than guessing.
- `methods` lists RPCs with unique identifier names and their two streaming
  flags. It is always present.
- `provenance.project` is the application, named exactly as the HTTP route
  inventory of the same build names it. `provenance.package` is the framework
  package that registered the service. `provenance.sourceKind` is closed:
  `grpc-server` serves `grpc`; `connect-gateway` and `api-bridge` serve
  `connect`.
- `name`, `transport` and `basePath` identify a service. Two entries with the
  same triple are `grpc_services.duplicate_service`, even when they are
  byte-identical; a producer collapses identical observations before it
  canonicalizes.

## Joining the HTTP route inventory

The parser validates only this document. Whether a route exists for a
Connect service is a join a consumer makes with the same build's
`putnami.http-routes.v1` inventory:

| `sourceKind` | Matching HTTP routes |
| --- | --- |
| `connect-gateway` | One `prefix` route whose `path` equals `basePath` and whose methods include `POST`. |
| `api-bridge` | One `exact` route per listed method, at `basePath` followed by the method name, whose methods include `POST`. |
| `grpc-server` | None. A native gRPC service is reached on the gRPC listener. |

## Canonical serialization and digest

Canonicalization performs these steps:

1. stamp the canonical schema and protocol identifiers;
2. sort each service's methods by name, and give every service a `methods` list;
3. sort services by `name`, then `transport`, then `basePath`, bytewise;
4. serialize `{ "protocol", "services" }` with fields in declared order,
   two-space indentation, and one trailing newline;
5. set `digest` to `sha256:` plus the lowercase SHA-256 hex of those bytes; and
6. serialize the full manifest with the same formatting.

Equivalent inventories therefore serialize byte-identically and produce the
same digest whatever the registration order. The golden under
`fixtures/equivalence/` pins the byte form.

## Diagnostics

Every code starts with `grpc_services.`: `parse_error`, `unknown_field`,
`invalid_protocol`, `invalid_schema`, `invalid_digest`, `digest_mismatch`,
`invalid_service_name`, `invalid_transport`, `invalid_base_path`,
`invalid_methods`, `invalid_method_name`, `duplicate_method`,
`missing_provenance`, `invalid_source_kind`, `source_transport_mismatch`,
`duplicate_service`, and `serialization_error`. Unknown fields, trailing JSON
values, and a missing or null `services`, `methods`, `methodsKnown` or streaming
flag are errors. A consumer must not apply part of an invalid inventory.

## Versioning and compatibility

The protocol identifier carries the major version. A v1 consumer accepts only
`putnami.grpc-services.v1` and fails closed on any other identifier. Adding a
diagnostic code is compatible. Changing a required field, a closed enum, the
identity triple, canonicalization or digest semantics needs a new major
identifier and schema URL. A producer that learns the methods of a source it
reports today with `methodsKnown: false` may report them with
`methodsKnown: true`: the wire shape is unchanged.

## Non-goals

- message types, request and response schemas, or protobuf descriptors;
- listener addresses, ports, TLS settings or any environment value;
- reachability or visibility, which the HTTP route inventory owns; and
- runtime discovery: the inventory is a build output.

## Ownership, support, and evidence

**Owner** — the `protocols` standardization layer. `protocols/grpc-services`
(`go.putnami.dev/protocol/grpc-services`) owns the wire, the schema and the
corpus; producers follow it.

**Support status** — `preview` in the workspace-root
[`putnami.support.json`](../../putnami.support.json) catalog, with
`default: true` and `parity: unsupported`; [`protocols/support`](../support/README.md)
defines the vocabulary. Every describe of an app that composes a gRPC surface
writes the inventory, so it is part of the default experience. As a `preview`
protocol it may break in a minor release whose release notes state the required
user change.

**Evidence for that status**

- One implementation, Go: the `go.putnami.dev/grpc` describe emitter.
- One active consumer in this tree: `@putnami/go` validates the document across
  the module boundary before describe promotes any schema. Its tests in
  `go/extension/internal/codegen/describe_test.go` refuse a malformed,
  unsupported-version, wrong-digest and non-canonical inventory, and prove the
  `go/go-project-toolchain#grpc-inventory-checked-before-promotion` requirement.
- `conformance_test.go` runs the corpus under `fixtures/`.
- `canonical_test.go` pins the golden bytes and digest, order independence, and
  input purity.
- `version_test.go` pins the identifier and schema URL together;
  `drift_test.go` pins the Go types and closed enums against the schema.

**What is missing for `stable`**

- A consumer across a boundary this repository does not control. The one
  consumer here validates the document and acts on none of its service facts.
- A second implementation that runs this corpus, or a reviewed parity decision
  beyond `parity: unsupported`.
- An entry in the
  [compatibility budget](../../tooling/cli/doc/21-compatibility-and-migration.md)
  and a stated N-1 read window.

## Feature and spec

The module owns the feature of its contract,
`grpc-services/service-inventory-contract`, specified in
[`specs/service-inventory-contract.json`](specs/service-inventory-contract.json).
The tests here prove the identifiers, the fixture corpus, the identity rules
and the canonical bytes. `go.putnami.dev/grpc` (`go/framework/grpc`) proves
under its own `the-go-grpc-…` checks that its server, gateway and API bridge
describe each source kind and its route join as the contract states, and that
its plugins collapse an identical registration and fail on a conflict.
`@putnami/go` (`go/extension`) proves under its own `the-go-extension-…` check
that describe refuses an inventory the parser refuses. Which surfaces the
framework composes, and when it describes them, stays in the producer's
`service-inventory` requirement in
[`go/framework/grpc/specs/grpc-services.json`](../../go/framework/grpc/specs/grpc-services.json).
The design intent is
[ADR 0001](doc/adr/0001-grpc-services-is-its-own-protocol.md).
