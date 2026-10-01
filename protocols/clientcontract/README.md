# First-party generated-client metadata

`clientcontract` defines version 1 of the `x-putnami-client` metadata carried by
Putnami-owned OpenAPI documents and operations. Standard OpenAPI and Proto
artifacts remain authoritative for operation identity and wire schemas. This
metadata preserves the service-client semantics those standards do not express
without loss.

This package is a protocol foundation. It publishes types, JSON Schemas, strict
validators, fixtures, and a generated-file manifest contract. Provider
projection, Go and TypeScript readers and emitters, service bindings, and
transport runtimes are not wired to this package yet.

## Document and operation metadata

A document marker has this shape:

```json
{
  "protocolVersion": 1,
  "service": {
    "id": "catalog.items",
    "audience": "urn:putnami:catalog:items"
  },
  "credentials": {
    "workload": {
      "kind": "service-token",
      "scopes": ["items:read"]
    }
  }
}
```

Its presence marks a first-party strict contract. Each operation records its
stream mode, logical stream message schemas, transports in preference order,
ordered security alternatives, declared errors, idempotency, and optional
resilience hints. A client takes the first security alternative its binding
satisfies; an empty `allOf` is the anonymous alternative and is always
satisfied. A credentialed alternative followed by an anonymous one is an
optional credential: presented when the binding holds it, omitted otherwise
(`go/framework/security/doc/adr/0002-an-optional-rule-serves-a-caller-that-presents-no-credential.md`). Credential profiles contain acquisition and injection
metadata only; tokens, API keys, client secrets, and other credential values
are invalid contract data.

The recursive schema subset preserves local references, typed maps, arrays,
exact enum/default JSON values, nullable values, discriminated unions,
bytes/binary formats, unsigned integer formats, numeric bounds, and read/write
annotations. Unsupported keywords, duplicate keys, ambiguous nulls, invalid
paths, unknown credential profiles, and incomplete transport or protobuf
metadata fail with stable diagnostic codes.

Connect and protobuf transports reference an exact
`/package.Service/Method`. The document projection records service methods and
streaming flags, field numbers and wire kinds, JSON names, presence, oneofs,
maps, and numeric enum values. Future readers therefore do not need to infer a
binary wire contract from OpenAPI property order.

WebSocket carries both transport metadata and the wire itself. The metadata is
the fixed public subprotocol `putnami.service.v1` and whether a safe server
stream declares gap-free resume; credential values are absent from both the
metadata and the subprotocol. The wire is
[`client-websocket-wire-v1.json`](schemas/client-websocket-wire-v1.json) plus the
strict frame parser and the published transition table in
[`websocket.go`](websocket.go): `NextWebSocketStateV1` answers every
(state, frame, direction, stream) triple, and `WebSocketConversationV1` adds the
session facts a single frame cannot carry — per-direction sequence continuity,
encoding agreement, and resume agreement between the `init` request, the `ready`
frame and the transport declaration. Every first-party runtime consumes that
table; none restates it. The state machine, admission rules and corrected
transitions are recorded in
[ADR 0002](doc/adr/0002-websocket-admission-uses-a-first-frame-state-machine.md).
The sockets themselves — the RFC 6455 framing, the provider upgrade handler and
the generated client transports — stay outside this foundation.

A WebSocket transport may instead declare `wire: "provider"`: the provider owns
the vocabulary of its messages. With `encoding: "binary"` each binary message is
raw octets and the operation declares no `messages` (a byte stream, such as a
database tunnel); with `encoding: "json"` each text message is one JSON value of
the declared `messages.input` or `messages.output`, under a `subprotocol` the
provider must name (such as `putnami.events.v1`). A provider-owned wire carries a
`bidirectional` stream, is the operation's only transport, never declares
`resume`, and never names a token in the `putnami.service.` namespace. It has no
envelope and no in-band admission: the upgrade request carries the declared
credentials. `wire` is absent on the first-party conversation, so its transports
keep their exact bytes. The rules, their diagnostic codes and the runtime
behavior both languages share are recorded in
[ADR 0010](doc/adr/0010-a-provider-owned-websocket-wire-is-declared-not-inferred.md).

A value the provider does not interpret has two closed spellings.
`{"x-putnami-json": "any"}` declares any JSON value; only `title` and
`description` may stand beside it. `{"type": "object", "additionalProperties":
true}` declares a JSON object with free-form values. The empty schema, any
other keyword value, a type or constraint beside the keyword, and an
`x-putnami-json` map value are refused. Generated clients carry these values
without interpreting them: `json.RawMessage` and `map[string]json.RawMessage`
in Go, `unknown` and `Record<string, unknown>` in TypeScript. They have no
Connect form. See
[ADR 0008](doc/adr/0008-opaque-json-is-a-declaration.md).

An operation that an external authority owns — a standard protocol a provider
serves beside its own routes, such as the OCI Distribution Specification —
carries `"x-putnami-external-contract": "<authority>"` and no
`x-putnami-client`. It stays in the document, and every first-party reader
skips it whole: it contributes no contract operation, no protobuf method and no
generated client method, and its own schemas are not held to the first-party
subset. Component schemas stay first-party, because the whole document shares
them. Both extensions on one operation (`client_contract.duplicate`), a marker
that is not a string (`client_contract.parse_error`) and a blank authority
(`client_contract.required`) are refused; an unmarked operation without
`x-putnami-client` stays refused. `ExternalContractAuthority` classifies one
operation, and `$defs.externalContract` in
[`x-putnami-client-v1.json`](schemas/x-putnami-client-v1.json) publishes the
value. See
[ADR 0011](doc/adr/0011-an-external-authority-can-own-an-operation.md).

`resilience.cache` declares a per-operation response cache: `freshMs`, an
optional `staleMs` that must exceed it, `maxEntries` and `keyFields`. Only a
unary operation whose idempotency is `safe` or `idempotent` may declare it, and
never in `defaults.resilience`; a key field must name an input the operation
declares. The key format, the forwarded-identity partition and the serve-stale
classes are shared by both runtimes and pinned by
[`fixtures/cache/keys.json`](fixtures/cache/keys.json). The runtimes partition
entries by forwarded user identity and service binding only; a tenant carried
any other way must be a declared input kept in the key, which the provider
owns. A generated manifest
repeats each operation's policy and lists `runtimeCapabilities:
["response-cache"]`; a manifest that declares a policy without that requirement,
or requires a capability the runtime of its language does not implement
(`RuntimeCapabilitiesImplementedBy`), fails with
`client_contract.unsupported_runtime_capability`. See
[ADR 0007](doc/adr/0007-a-response-cache-is-declared-per-operation.md).

`resilience.cache.invalidationFields` names top-level properties of the JSON
success body that tag each stored answer. Each must be a `string`, `integer` or
`boolean` property in every success body that declares it. A string declared
`format: byte` or `binary` is octets, not a string: a TypeScript runtime
decodes it to a byte array no value can equal, so it is refused. A consumer then
drops every answer carrying one value, across the service's operations and for
every forwarded identity: `InvalidateResponsesByField(serviceID, field, value)`
in Go, `invalidateResponsesByField(field, value)` in TypeScript. A caller that
needs the provider's current answer bypasses the cache for one call:
`client.WithoutResponseCache(ctx)` in Go, `{ withoutResponseCache: true }` in
TypeScript. A bypassed call neither reads, stores nor joins a call in flight.
The value rendering and the property schemas a field may name are pinned by
[`fixtures/cache/invalidation.json`](fixtures/cache/invalidation.json). See
[ADR 0007](doc/adr/0007-a-response-cache-is-declared-per-operation.md).

A consumer binding may carry static non-secret request headers beside its
credentials: `ServiceBinding.Headers` in Go, `ServiceBinding.headers` in
TypeScript, and `clients.services.<id>.headers` in the shared configuration.
[`fixtures/binding/headers.json`](fixtures/binding/headers.json) pins what both
runtimes accept, how they canonicalize it, what they refuse with `client.config`,
and the exact reserved name and prefix sets. See the
[Go client ADR 0007](../../go/framework/client/doc/adr/0007-static-binding-headers-are-non-credential-defaults.md).

Transport entries describe provider alternatives; they do not assert that a
current language runtime implements each cardinality or reconnect behavior. A
future target emitter must fail explicitly when it cannot execute a declared
alternative without loss.

An operation's own `resilience.stream.reconnect` always counts; the document
default reaches only server streams, so it never refuses a unary operation. An
effective reconnect requires a first-party `websocket` transport that declares
`resume` or an `sse` transport that declares a continuation; a provider-owned
wire never satisfies it.

An `sse` transport of a safe server stream may declare how a broken connection
continues. `{"sse": {"continuation": {"mode": "cursor", "cursor":
{"outputField": "cursor", "queryParameter": "cursor"}}}}` reopens after the
position of the last message the consumer received. That position is an opaque
string that each message carries in a required output field, and the reopened
connection sends it back in a declared query parameter. The provider continues
exclusively after it, on any instance. `{"mode": "best-effort"}` reopens the
original query; messages may be missing or repeated, and it is never lossless.
Only an operation that declares a continuation negotiates the SSE wire
`X-Putnami-Stream-Wire: putnami.sse.v1`, which ends with the exact terminal
`event: complete\ndata: {}\n\n`. On that wire, an end of body before the
terminal is an interruption, not a success. Every other SSE stream keeps its
legacy framing. A generated target that declares a continuation requires the
`sse-continuation` runtime capability, which both the Go and the TypeScript
runtimes implement. The declaration rules, the delivered
position, negotiation, mixed-version behavior and session bounds are in
[ADR 0013](doc/adr/0013-an-sse-stream-declares-how-it-continues-and-a-negotiated-stream-ends-explicitly.md).

## Schemas and conformance corpus

[`x-putnami-client-v1.json`](schemas/x-putnami-client-v1.json) is the published
schema for document and operation metadata. The executable OpenAPI corpus is
under [`fixtures/openapi`](fixtures/openapi): `full.openapi.json` exercises every
metadata family, `external-operation.openapi.json` pins an operation an external
authority owns — its own schema is one the subset refuses, so the fixture reads
only when the reader skips it — and the invalid fixtures pin strict rejection
behavior.

The WebSocket corpus is under [`fixtures/websocket`](fixtures/websocket). Each
scenario declares the operation, stream mode, encoding, resume capability and
frame bounds, then replays an ordered list of directed frames through the
published conversation; the valid scenarios must reach a terminal frame and the
invalid ones must produce the diagnostic code
[`expectations.json`](fixtures/websocket/expectations.json) names.

The SSE corpus is under [`fixtures/sse`](fixtures/sse).
[`wire.json`](fixtures/sse/wire.json) pins the negotiation header and token, the
exact terminal bytes, how both sides read the header, and what a provider writes
for each request. [`scenes.json`](fixtures/sse/scenes.json) replays one
connection per scene. Each scene gives the declared continuation, whether the
runtime asked for the negotiated wire, the acknowledgment, the body chunks and
how the transport ended. It expects the outcome, the delivered messages, the
delivered position and the reopened query. Old and new runtimes against old and
new providers are all covered. `sse_wire_test.go` replays both files against
the exported wire vocabulary in [`sse.go`](sse.go). Each provider and runtime
that implements ADR 0013 must replay the same files against real connections.

[`fixtures/ir/full.ir.json`](fixtures/ir/full.ir.json) is a canonical neutral
projection for future cross-language reader conformance. Keeping it next to the
source OpenAPI fixture establishes the expected lowering boundary; it does not
claim that a Go or TypeScript production reader is already integrated.

Every generated target will write `client.putnami.json`, described by
[`generated-client-manifest-v1.json`](schemas/generated-client-manifest-v1.json).
The manifest binds an exact provider contract digest to the generated service,
registration symbols, operations, ordered transports, and exact file digests.
A target that leaves operations out by configuration — a Go target's
`go.omitOperations` — names them in `omittedOperations`, in ascending order, so
`operations` and `omittedOperations` together account for every contract
operation and a reader of committed bytes alone can tell an omission from a lost
operation. The key is absent when the target generates every operation.
This package validates the manifest shape and rejects unsupported generator
markers, unsafe paths, operations without a matching binding client, duplicate
inventory entries, an operation both generated and omitted, and non-canonical
inventory order. The marker and digests
are structural declarations; authenticating them against provider and file
bytes belongs to future workspace integration.

Two inventories carry the exceptions to first-party generation, and this
package publishes their schemas so the strict parser and the documents a
repository commits cannot drift apart. Each project commits its own copy of
each inventory in its directory, and the directory names the project, so no
entry does.
[`clientgen-external-v2.json`](schemas/clientgen-external-v2.json) describes
`<project>/clientgen.external.json`: for each unmarked third-party contract, its
authority, adapter, exact normalized transport callsites, owner, contract tests
and reason. [`clientgen-framework-v2.json`](schemas/clientgen-framework-v2.json)
describes `<project>/clientgen.framework.json`, the separate inventory for the low-level
calls that IMPLEMENT the generated binding runtimes; its `runtime` is restricted
to `@putnami/client` and `go.putnami.dev/client` so the implementation boundary
cannot become a consumer-wide waiver. Both are closed objects and both key
authority on a callsite fingerprint rather than a file or a directory, so an
entry exempts one expression and not its neighbours. The version 1 schemas,
[`clientgen-external-v1.json`](schemas/clientgen-external-v1.json) and
[`clientgen-framework-v1.json`](schemas/clientgen-framework-v1.json), describe
the older layout: one workspace-root file whose entries each name their
project. clientgen reads none of their entries and lists the project file each
one moves to.

## Conventions the corpus encodes

Three cross-cutting decisions are written down here because the contract owns
the vocabulary, and the corpus is guarded against drifting from them.

- **Error codes** are the dotted snake_case vocabulary of `go/framework/errors`
  — `not_found`, `unavailable`, `http.bad_request` — in both languages and on
  every transport
  ([ADR 0003](doc/adr/0003-stable-error-codes-are-the-go-framework-vocabulary.md)).
- **Integer width is declared, never inferred**: every `type: "integer"` schema
  carries a `format` from `int32 | int64 | uint32 | uint64` and exact bounds, and
  a contract `int` is 64 bits on every transport
  ([ADR 0004](doc/adr/0004-integer-width-is-declared-not-inferred.md)).
- **Stream sessions have five phases and four budgets**, with admission defined
  identically for SSE, WebSocket, Connect and unary REST. This is where
  `resilience.stream.handshakeTimeoutMs` comes from
  ([ADR 0005](doc/adr/0005-stream-sessions-have-five-phases-and-four-budgets.md)).

## Compatibility

This package reads exactly protocol version 1 and writes no compatibility
aliases. Version 0, missing versions, and versions newer than 1 are rejected;
there is no migration because no earlier `clientcontract` release exists. A
provider or generated manifest with another version must be regenerated by a
reader and writer that explicitly support the same version. Adding a field or
closed enum value requires a protocol-version decision and coordinated consumer
support before adoption.

## Support and adoption

- **Status**: `preview` — recorded in the workspace-root
  [`putnami.support.json`](../../putnami.support.json). The contract is
  documented, runs under the repository gate, and is the default path of
  first-party client generation. It is not `stable`: every consumer is still
  inside this workspace, and adding a field or a closed enum value remains a
  protocol-version decision rather than an additive edit.
- **Owner**: `go.putnami.dev/protocol/clientcontract`
  (`protocols/clientcontract`).
- **Adoption**: both providers project it — `go.putnami.dev/api` and
  `@putnami/application`; both readers parse it with the same closed diagnostic
  vocabulary; both emitters generate from it; and both runtimes —
  `go.putnami.dev/client` and `@putnami/client` — dispatch on the transports it
  declares.
- **What may still change**: the protocol version stays the exact integer 1, so
  a new field, a new closed enum value or a new discovery location is a version
  bump with coordinated consumer support, never an additive edit. Wire v1 keeps
  `proto` in the WebSocket encoding vocabulary while both generators refuse it;
  carrying it is a later version's decision.
- **Evidence**: strict Go parsing and validation, published JSON Schemas, the
  valid/invalid OpenAPI corpus replayed by both readers, version-window tests,
  schema/type drift tests, generated-manifest round-trip, marker, path, and
  binding-consistency tests, and the WebSocket wire: the frame parser, the
  published transition table, the scenario corpus, and the schema-to-Go drift
  guard over frame types, cancel codes and payload encodings.

## Shared page transports

`PageQuery`, `Page[Rows]` and `PageTransportSchemas()` declare the common
snapshot query and envelope once. Use `Page[json.RawMessage]` for a generic
transport, and a domain row type for an owner. In Go provider declarations,
name the instantiation (`type SnapshotPage clientcontract.Page[Rows]`) and
publish `api.Type[SnapshotPage]()` so generated schema names stay plain. A domain pins its resolved
schemas with `ValidatePageTransportSchemas(query, envelope)`; the TypeScript
twin is `pageTransportSchemas` / `validatePageTransportSchemas` from
`@putnami/client/generator`. Query names are exported as `QueryParamRelation`,
`QueryParamAfterKey` and `QueryParamLimit` on both sides.

Leave `OwnerConfirmedAt` at its zero value when an owner cannot provide a
confirmation clock; the JSON property is omitted. A zero `Watermark` stays
present because it is part of every page.

The [shared page decision](doc/adr/0012-a-shared-page-protocol-is-bound-by-the-consumer.md)
defines cursor termination, int64 watermark and authoritative clock semantics,
owner bounds and caller binding lifetime.
