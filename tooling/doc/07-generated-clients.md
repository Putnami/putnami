# Generated Clients

A Putnami provider declares each operation once. The workspace turns that
declaration into typed Go and TypeScript clients, keeps them synchronized, and
fails the gate when a consumer bypasses them.

This page is the workspace half of the path. The declaration and call syntax
live with each language: [Service Clients](/docs/frameworks/go/service-clients)
for Go, [Smart Client Library](/docs/frameworks/typescript/smart-client) for
TypeScript.

```
declare the provider  →  generate  →  register the binding  →  call
                         putnami clientgen
                         putnami clientgen-sync
                         putnami clientgen-check
```

## Opting a provider in

A provider lists `/tooling/clientgen-extension` in its `putnami.json`
`extensions`, then declares its targets — `api.Clients(...)` in Go,
`clientGenerator(...)` in TypeScript. The provider build writes
`.gen/clientgen/config.json` and the marked OpenAPI and Proto artifacts; those
are the only seam between the two toolchains.

The same-language client is emitted in-app during `putnami build`. The
cross-language client is emitted by `putnami clientgen`, which runs the other
toolchain's emitter against the provider's own contract. No extension shells out
to the other.

A generated client is importable from every scope of the workspace, with
nothing to declare: it is the service's published way in. This holds when the
client's `client.putnami.json` names a service a provider in the workspace
commits, and the client's `putnami.json` declares no `visibility`. A client
that declares `"visibility": "scope"` keeps that boundary.

## The three workspace commands

They operate on every project in the Putnami project index, regardless of an
impacted consumer-only selection.

| Command | Result |
| --- | --- |
| `putnami clientgen` | Generate the configured targets for the selected providers. |
| `putnami clientgen-sync` | Build all provider contracts without cache reuse, generate every configured target, synchronize tracked files, rebuild every project as a compile gate, and emit the coverage, lineage and adaptation report. |
| `putnami clientgen-check` | Judge every committed client against its committed manifest and the committed provider contract, and fail on missing coverage, forged or extra generated files, an unclassified external contract, or a handwritten first-party transport. It builds nothing and renders nothing. |
| `putnami clientgen-adopt` | Rewrite authored imports and construction onto generated bindings, for the entries two emitter manifests can prove on their own. |

Sync and adopt build the provider contracts through the Putnami that launched
them, so an impacted consumer-only selection cannot leave a provider on a stale
contract. The check spawns no Putnami.

## Drift is the generator's verdict

Whether a committed client is what the current contract generates is decided
where the client is written. The tasks that write committed clients —
`clientgen-go`, `clientgen-ts`, `@putnami/go` `build-describe` and
`@putnami/typescript` `build-generate` — declare `drift: "fail"` on their
client output. The engine compares what each writes (or what a cache hit
restores) with the bytes present immediately before, and fails the task with
the diagnostic `generated-output-drift` naming the changed files. The worktree
then holds the regenerated client; commit it. A CI checkout of stale committed
bytes fails the same way from the cache restore.

## The validate guard

`clientgen-check` is contributed to `putnami validate` with `workspace-once`
activation, so the gate every project already runs verifies the whole
workspace's committed clients exactly once — whatever the selection resolves
to, and whether or not the selected projects declare the extension. A workspace
opts out the ordinary way, by disabling the extension or the job.

`validate` waits on the `!clientgen` session barrier: every selected provider
regenerates its targets in place, under the engine's drift judgment, before the
guard reads them; a consumer-only selection plans no generation. The guard
itself reads committed inputs only — the project index, each provider's
committed `schema/openapi.json`, its committed `client.putnami.json` manifests
and the two root inventories — so a cold clone and a tree the session just
built reach one verdict. HEAD is never consulted: what CI builds, ships and
caches is the worktree.

### What the guard refuses

| Refusal | What to do |
|---|---|
| A generator task reports `generated-output-drift` | The worktree already holds the regenerated files: commit them |
| A generated file was hand-edited (its hash no longer matches the manifest), or an extra file sits in a generated directory | Revert it; the directory is a Putnami-owned output |
| A committed manifest was cut from a contract other than the committed one | Select the provider (`putnami validate --projects <provider>`) or run `putnami clientgen-sync`, then commit |
| A first-party operation has no generated client | Declare the target on the provider, then sync |
| A provider declares operations and commits no `client.putnami.json` (`clientgen.missing-manifest`), or names no generation contract at all (`clientgen.missing-config`) | Generate and commit the target — unless the contract is empty by design (below) |
| A manifest's `omittedOperations` names an operation the provider does not declare (`clientgen.omitted-operation-drift`) | Remove the name from `go.omitOperations`, then sync |
| A handwritten transport call reaches a first-party provider | Replace it with the generated binding, or classify it (below) |
| An unmarked OpenAPI document is generated without `thirdParty: true` | Mark the provider, or declare the contract external |

Every transport callsite the scanner finds is either associated with a
first-party provider — which fails, because a generated binding must replace it
— or claimed by one of the two inventories the project that holds the callsite
commits in its own directory: `<project>/clientgen.framework.json` and
`<project>/clientgen.external.json`. A project's file is `protocolVersion: 2`
and its entries name no project: the directory does. `protocolVersion: 1`, one
root file whose entries each name their project, is the older layout: at the
workspace root it fails with the project file each entry moves to, and in a
project directory it fails. A version 2 file at the workspace root is read only
when a project lives at the root.

`clientgen.framework.json` covers a callsite no generated binding can replace,
under one of three statuses:

- **`framework-runtime`** — the transport a generated binding itself dispatches
  to. Only `@putnami/client` and `go.putnami.dev/client` may claim it.
- **`transport-primitive`** — a generic transport whose endpoint and contract
  come from the caller: a form submission, a markdown component, a test harness.
- **`pending-provider-contract`** — a first-party Putnami service whose
  provider-side client declaration does not exist yet. It must name the
  operations it waits on and the work that closes it, so it is a versioned state
  and not an exemption.

`clientgen.external.json` covers an adapter that speaks a contract someone else
owns — S3, Google Cloud Storage, the GCE metadata server, OAuth 2.0 and OIDC,
OTLP, the OCI distribution spec, the Go module proxy protocol, the npm registry
API — and names that authority.

Both inventories claim exact callsite expressions. An entry never names a
folder, a file or a transport as a class: a second call of the same symbol in a
listed file is a new callsite and fails, and an entry that matches nothing fails
too, so an inventory can only shrink.

### A contract that is empty by design

A provider may declare its service identity and no first-party operation at
all: one whose every route names the authority that owns it
(`x-putnami-external-contract`), or which serves none. A Go module proxy or an
npm registry is the usual shape — it only speaks a standard protocol, so there
is nothing to generate a client for, and `go.putnami.dev/api` stages no target.

The guard reads that as a satisfied declaration. It raises no
`clientgen.missing-manifest` for a declared target that never materialized, no
`clientgen.missing-config`, and no `clientgen.no-targets`, and it counts nothing
as required. The declaration lives in the provider's committed
`schema/openapi.json` — the document-level `x-putnami-client` marker with an
empty first-party operation set — so a cold clone reaches the same verdict as a
tree the session just built. No flag declares it: the operation set does.

Emptiness is exact, not a suppression. A contract that declares even one
first-party operation still owes its declared targets a generated client, and a
contract whose operations the guard could not read — an operation with no
`x-putnami-client` metadata, an invalid marker, a missing `operationId`, an
`x-putnami-external-contract` that names no authority — is unreadable, never
empty, and keeps failing. A target that IS committed is judged in full, so a
client left behind after its last operation moved to an external authority fails
as `clientgen.operation-coverage-drift`.

Such a provider reports no target, because no target of it can carry a call. A
handwritten transport that reaches it is still a failure, and its diagnostic
says what actually closes it:

```
handwritten first-party transport to gomod-server must be replaced by the
generated binding: gomod-server declares no first-party operation, so no Go
client is generated for it; classify this callsite in
consumer/clientgen.external.json under the authority that owns the wire
```

See `tooling/clientgen-extension/doc/adr/0004-an-empty-first-party-contract-is-a-declaration.md`.

## Migrating a handwritten client

1. **Get the provider contract first.** Nothing can be generated until the
   provider declares its operations and its client targets. Until it does, the
   consumer's callsites are `pending-provider-contract` in the consumer
   project's `clientgen.framework.json`, naming the operations they wait on.
2. **Generate.** `putnami clientgen-sync` writes the client package and its
   `client.putnami.json` manifest.
3. **Adopt what the manifests can prove.** `putnami clientgen-adopt` rewrites a
   generated package that moved, and the client constructor or registration call
   the product contract promises for a service. Both sides need identical
   contract bytes, service and operations. Go rewrites a qualified selector on
   the exact identifier the file binds to that import; TypeScript rewrites the
   specifier and its uses, and refuses a source where the name is also bound
   locally or used in object-shorthand position. Every file is staged before any
   file is written, so an adoption that cannot finish migrates nothing.
4. **Work the queue.** Argument, credential and call-shape changes are never
   inferred. The report's adaptation entries name the generated package and
   binding symbols; the edit is yours.
5. **Delete the inventory entry.** The `pending-provider-contract` entry that
   covered the old callsite now matches nothing, and an entry that matches
   nothing fails the check — which is how the state closes itself.

## Strict generation diagnostics

First-party generation is strict. A semantic the shared client IR cannot carry
fails with the operation named, rather than emitting a client that silently
loses it. Both emitters raise `clientgen_unsupported_semantic`; the Go code is
`api.clientgen_unsupported_semantic`.

### Declaration shape

| Diagnostic | Cause | Remedy |
|---|---|---|
| `declares a request body on GET` (or `HEAD`) | RFC 9110 leaves a GET/HEAD payload undefined and every intermediary may drop it | Move the payload to query parameters, or change the method |
| `SSE request body is not supported` | A server stream declared a request body | Carry the input in path or query parameters |
| `declares stream shape …, which this generator does not emit` | The stream mode is not `server`, `client` or `bidirectional` | Declare one of the three shapes |
| `<shape> operation over <transports>` | No declared transport can carry that shape — for example a client stream without `websocket` | Add a transport that carries the shape, or change the shape |
| `missing operation contract` | The route carries no `x-putnami-client` operation metadata | Register the endpoint through the api plugin so the contract is projected, or, for a standard protocol leg, mark it `External` / `external` |
| `declares both x-putnami-client and x-putnami-external-contract` (`client_contract.duplicate`) | One operation claims to be first-party and owned by an external authority | Keep one: drop `External` / `external`, or the first-party metadata |
| `x-putnami-external-contract must name the external authority` (`client_contract.required`) | The marker names a blank authority | Name the specification that owns the wire contract, for example `OCI Distribution Specification v1.1` |
| `declares External … together with …` / `external … is declared together with …` | `External` / `external` is combined with another client option | Drop the other options: an operation an external authority owns publishes no first-party policy |
| `declares External …, and this API publishes no first-party client contract` | `External` / `external` on an API without `api.WithClientService` / `api({ client })` | Declare the client service, or drop `External` |

### Transports

| Diagnostic | Cause | Remedy |
|---|---|---|
| `websocket encoding "proto"` | Wire v1 keeps `proto` in the contract; no first-party codec carries WebSocket messages as protobuf | Declare `json` encoding on the WebSocket transport |
| `declares a websocket transport without the putnami.service.v1 subprotocol` | The transport does not speak the published first-party wire | Declare `api.WithClientService` / `api({ client })` so the route negotiates the subprotocol, or declare a provider-owned wire |
| `must declare both an input and an output message schema` (provider-owned wire) | A provider-owned subprotocol carries typed JSON frames both ways | Declare `Body`/`.body` and `Returns`/`.returns` as `Stream` of the frame types, or declare a byte stream |
| `declares websocket resume, which only a server stream can honor` | `resume` on a client or bidirectional stream would replay the caller's own messages | Remove `resume`, or make the operation a server stream |
| `stream reconnect without a provider-declared resume transport` | `resilience.stream.reconnect` asks for a continuation the provider cannot serve | Declare `resume: true` on the WebSocket transport, or drop `reconnect` |
| `connect encoding <x>` | The Connect transport declares an encoding neither runtime carries | Declare `json` or `proto` |
| `client_contract.invalid_transport` on a `wire: "provider"` transport | A provider-owned wire beside another transport, on a stream that is not bidirectional, or under a token that is malformed or in the `putnami.service.` namespace | Keep the provider wire as the operation's only transport, on a bidirectional stream, under the provider's own token (ADR 0010) |
| `a connect transport without a fully-qualified protobuf method path` | The descriptor does not name the RPC | Mount the Connect bridge so the provider publishes the method path |
| `connect encoding "proto" without a published protobuf descriptor` | Nothing describes the messages on the wire | Add the `proto` plugin so the descriptor travels in the contract |
| `a connect transport with more than one declared success status` | A Connect response envelope carries one success | Declare one primary success status |
| `declares a Connect encoding order and publishes no Connect transport` | `ConnectEncodings` / `connectEncodings` is set on an operation whose provider mounts no Connect bridge | Mount the bridge (`grpc()`, `PublishClientConnectTransport`), or drop the encoding order |
| `declares Connect encoding <e> twice in its client encoding order` | The same encoding is named twice | Name each encoding once |
| `declares Connect encoding <e>, which the mounted bridge does not serve` | The order names an encoding the provider does not serve for this route | Serve it, or narrow the order to the encodings the bridge publishes |

### Connect and protobuf semantics

| Diagnostic | Cause | Remedy |
|---|---|---|
| `declares repeated query parameter <p>` on Connect | A Connect request envelope carries one value per parameter | Declare the parameter once, or drop the Connect transport |
| `declares a non-object request body` on Connect | A Connect envelope carries the body as an object | Wrap the value in a declared object schema |
| `declares a nullable member` over `connect+proto` | proto3 has one absence, so null and absent would become one value | Remove `nullable`, or drop the `proto` encoding |
| `property "<name>", whose protobuf json_name would rename it` | The property name does not survive the proto snake/camel round trip | Rename the property to one that does |
| `an opaque JSON value (<field>), which protobuf cannot carry without loss` | `google.protobuf.Value` holds every number as a double | Drop the Connect transport; the Go provider already declares REST only for such a route |

### Schemas

| Diagnostic | Cause | Remedy |
|---|---|---|
| `declares type "integer" without an int32\|int64\|uint32\|uint64 format` | Integer width is declared, never inferred | Declare the format and the exact bounds |
| `has external $ref` / `references missing <name>` | The schema points outside the document | Inline the schema, or declare it in the same contract |
| `array has no items` | An array with no element schema is untyped | Declare the element schema |
| `mixes named properties and untyped additionalProperties` | An object with named properties and free-form members has no one generated type | Declare the named properties, or a free-form object (`additionalProperties: true` alone) |
| `an opaque JSON value as parameter <p>` | A parameter travels as text; an opaque value has no text form both runtimes agree on | Carry the value in a JSON body |
| `has no representable schema type` | The schema declares no type the IR can carry | Declare a supported type |
| `generated symbol <s> collision between <a> and <b>` | Two operations or schemas would emit the same symbol | Rename one operation or schema |
| `request content types other than one application/json representation` | Content negotiation is not part of a generated client | Declare one JSON representation, or declare the body binary |
| `response <status> content types other than one application/json representation` | Same, on the response | Declare one representation per status |

### Raw octets

| Diagnostic | Cause | Remedy |
|---|---|---|
| `declares raw octets without a positive x-putnami-max-bytes bound` | An unbounded octet body cannot be sized by any consumer | Declare `maxBytes` |
| `declares raw octets under a JSON media type` | Octets need their own media type | Declare `application/octet-stream` or another non-JSON media type |
| `declares format "binary" inside a JSON document` | A JSON string cannot be raw octets | Declare base64 bytes as `format: "byte"`, or declare the whole body binary |
| `declares nullable raw octets` | An absent body is an empty one | Remove `nullable` |
| `constrains raw octets with JSON schema vocabulary` | `enum`, `oneOf`, `properties` and `items` describe JSON, not octets | Remove the JSON keywords |
| `a raw octet payload on a <mode> stream` | Streams carry declared messages, not bodies | Declare a unary operation, or carry the payload as declared messages |
| `dispatches on <transport>; only rest-json carries octets unchanged` | A Connect envelope would base64-wrap the octets | Declare `rest-json` first |
| `a raw octet payload beside multiple declared success variants` | The caller could not tell which variant carries octets | Declare one success status |
| `response <status> mixing raw octets with another representation` | Same response, two representations | Declare one representation |
| `response <status> declaring typed headers beside raw octets` | Typed response headers are a JSON-envelope feature | Drop the declared headers |
| `a raw octet request mixed with another representation` | Same, on the request | Declare one representation |

### Leaving an operation out of the Go target

`go.omitOperations` — `GoClientOptions.OmitOperations` in `api.Clients`, `go: {
omitOperations }` in `clientGenerator` — names operations, by `operationId`,
that the Go target leaves out instead of failing the whole provider. Use it for
an operation the Go emitter cannot represent yet; the default stays a failure.
Each operation left out is named in the generated client's doc comment, in the
build output, and in `omittedOperations` of the committed `client.putnami.json`.
The guard reads that list from the committed manifest, so a cold clone judges
the omission exactly as a built tree does: the operation stays required in the
coverage count and is never covered. A name the contract does not declare fails
generation.

An operation with several success statuses is no longer a reason to leave it
out: the Go client returns a result carrying the answered status (see the
[Service Clients](/docs/frameworks/go/service-clients) page).

### Provider mode

| Diagnostic | Cause | Remedy |
|---|---|---|
| `clientgen_first_party_required: provider client generation requires x-putnami-client` | An unmarked OpenAPI document reached first-party generation | Declare the provider through the api plugin, or set `thirdParty: true` for a contract Putnami does not own |

A first-party Putnami route may not be placed on the external list merely
because it uses a non-REST transport.

## Determinism and caching

Both project tasks are cacheable, so their cache keys name every input that
decides an emitted byte: the clientgen configuration, the built contract, the
provider `package.json` and — for the TypeScript target — the Biome
configuration, its `extends` chain and the EditorConfig beside it, because the
emitter canonicalizes every file it writes with Biome. A configuration this
resolution cannot name as a workspace path fails generation instead of being
silently left out of the key.

Two runs of the same contract produce the same bytes. That is what makes drift a
gate failure rather than a diff to review.

## Reference

- `tooling/clientgen-extension/README.md` — the extension's own contract, the
  inventory schemas, and the execution model
- `protocols/clientcontract/README.md` — the versioned metadata and manifest
  formats, and the published WebSocket wire
