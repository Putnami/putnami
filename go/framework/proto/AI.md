# go.putnami.dev/proto

Renders `.proto` from `api.Plugin` discovered routes.

## Surface

- `proto.NewPlugin(proto.PluginOptions{PackageName, GoPackage, Output})` — constructor
- `proto.Plugin.From(*api.Plugin)` — wires auto-discovery
- `proto.Plugin.Document()` — accessor for the rendered Document
- `proto.Plugin.Describe(*app.DescribeContext)` — implements `app.Describer`; writes the proto under `ctx.OutputDir/schema/api.proto` for build-time codegen
- `proto.Plugin.WriteTo(path string) error` — explicit export; an empty path uses `PluginOptions.Output`. No-op before `Configure`
- `proto.Generate([]api.DiscoveredRoute, Options) Document` — pure generator (no plugin)
- `proto.Document` — `{Content, PackageName, Imports, Service, Messages,
  GenerationErr}`. `GenerationErr` is set for a declaration proto3 cannot carry;
  `Content` stays empty and `Plugin.Configure` returns the error
- `proto.Document.RouteMethods()` — `"<METHOD> <path>"` →
  `/package.Service/Method`, the single join between a route and its Connect
  method identity. `proto.RouteKey(method, path)` builds the key
- `proto.ClientDescriptor(Document)` — the shared
  `clientcontract.ProtobufDescriptor` a Connect client decodes with
- `proto.Service`, `proto.RPC`, `proto.Message`, `proto.Field` — rendered shapes.
  A `Field` carries `Kind` (`proto.FieldKindScalar|Message|Enum|Map`), `Number`,
  `Repeated`, `Optional` (explicit proto3 presence), `OneOf` and `Map`
- `proto.DefaultOutputPath` (`schema/api.proto`) and `proto.FallbackOutputPath`
  (`.gen/schema/api.proto`)

## RPC name derivation

`clientcontract.RPCName(method, path)`, the base name every emitter derives:

- `GET` without path param → `List<Subject>`
- `GET` with path param → `Get<Subject>`
- `POST` → `Create<Subject>`
- `PUT` / `PATCH` → `Update<Subject>`
- `DELETE` → `Delete<Subject>`
- `Subject` is the camel-case concatenation of non-`{...}` path segments.

## Type mapping

`reflect.Kind` → proto:

The descriptor states exactly what the published JSON schema states — see
`doc/adr/0002-a-descriptor-states-the-wire-shape-the-schema-states.md`.

- `String` → `string`
- `Bool` → `bool`
- `Int8 / Int16 / Int32` → `int32`, `Int / Int64` → `int64`
- `Uint8 / Uint16 / Uint32` → `uint32`, `Uint / Uint64` → `uint64`
- `Float32` → `float`, `Float64` → `double`
- `[]byte` / `[N]byte` → `bytes` (one base64 JSON value, not a list of numbers)
- Other slices → `repeated <element>` (both struct fields and a top-level `[]T`
  request or reply root, which becomes `repeated <element> value = 1`)
- `map[string]T` → `map<string, T>` with its exact key and value kind
- `time.Time` → `google.protobuf.Timestamp`, `time.Duration` →
  `google.protobuf.Duration` (the matching `import` is added automatically)
- Structs → nested `message <Name>` (registered once, by struct identity);
  a struct with no exported field is an empty message, matching the empty closed
  object the JSON schema describes

Field selection uses the same selector as the JSON schema and the body
validator, so an embedded struct is flattened exactly as `encoding/json`
flattens it.

## Presence

A field carries explicit proto3 presence (`optional`) when — and only when — the
published JSON schema lets the property be absent (no `validate:"required"`) or
be null (a pointer). Repeated fields and maps are absent-as-empty on both wires
and proto3 forbids marking them optional.

## Refusals

Generation fails, rather than degrading a declaration, for:

- a map keyed by anything but a string,
- a nested list (`[][]T`) — proto3 has no `repeated repeated`,
- a map of lists or a map of maps.

A route whose sections reach `json.RawMessage` or `any` (opaque JSON) is left
out of the descriptor rather than refused: `google.protobuf.Value` holds every
number as a double, so there is no lossless proto3 form. The route keeps REST,
the bridge mounts no Connect URL for it, and the contract declares no Connect
transport — the rule a raw octet payload follows.

The message names the declaration and the remedy. Nothing is emitted as a
`bytes` placeholder: that placeholder is what made a Connect client decode
base64 where a REST client decoded an object.

## Output validity

The renderer keeps its output protoc-valid even for awkward Go input:

- Emitted identifiers (field/message names) are sanitized to the proto grammar
  `[A-Za-z_][A-Za-z0-9_]*`; an invalid `json` tag like `"my field"` becomes
  `my_field`. Already-valid identifiers are unchanged.
- Multi-line endpoint descriptions emit one `//` comment per line, so a newline
  can't break out of the comment into the service body.

## Streaming RPCs

Endpoints declared with `api.Stream` / `api.StreamOf[T]()` (server, client, or
bidirectional) emit `stream` modifiers on the proto Request and/or Reply per
`DiscoveredRoute.StreamMode`. Non-streaming HTTP methods produce unary RPCs.

## Lifecycle

1. `apiPlugin.Configure()` runs first (must be registered before the proto plugin).
2. `protoPlugin.Configure()` reads `apiPlugin.DiscoveredRoutes()` and renders the
   proto document **into memory**. It writes nothing, at runtime or under
   describe mode — the two paths are identical, so a served workload never writes
   into its working directory and never fails to start because that directory is
   read-only.
3. Build-time describe mode calls `Describe(ctx)`, which writes
   `<ctx.OutputDir>/schema/api.proto`; the runner copies it into the project tree
   (skipped when `options.generate.schema=false`).
4. `WriteTo(path)` is the explicit export for callers outside `putnami build`; an
   empty path uses `PluginOptions.Output`. `Output` affects nothing else.
5. `Document()` exposes the in-memory result for tests and tooling.

Routes finalized with `api.Endpoint(...).Document()` are **skipped**: the proto
service maps api-plugin handlers, and a document-only endpoint has none, so an
emitted RPC would advertise a URL that cannot answer. Those routes still appear
in the OpenAPI document.

## Build-time generation

The Putnami Go extension's `describe` phase compiles a host binary, runs it with
`PUTNAMI_DESCRIBE=all PUTNAMI_DESCRIBE_OUT=<.gen>`, and copies `schema/api.proto`
into the project tree (skipped when `options.generate.schema=false` in
`putnami.json`). No need to start the app to commit the proto.

## Maintained contract

This public package is `stable` in the workspace [support
catalog](../../../putnami.support.json). It renders one half of the
[`go/api-contracts`](../api/putnami.features.json) feature, whose
[specification](../api/specs/api-contracts.json) is owned by
[`go.putnami.dev/api`](../api); this package's own durable decision is the
[build-output ADR](doc/adr/0001-contract-artifacts-are-build-outputs.md).

`Configure` writes nothing. `Describe` produces the committed artifact and
`WriteTo(path)` is the explicit export (an empty path uses
`PluginOptions.Output`). Before v1.0, follow the workspace [migration-based
compatibility policy](../../../RELEASE.md); do not infer strict compatibility
between every `0.x` minor.
