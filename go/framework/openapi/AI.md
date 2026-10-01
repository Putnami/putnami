# go.putnami.dev/openapi

OpenAPI 3.0.3 spec generation from registered HTTP endpoints and Go structs.

## Quick Start

```go
import (
    "go.putnami.dev/api"
    "go.putnami.dev/app"
    "go.putnami.dev/http"
    "go.putnami.dev/openapi"
)

server := http.NewServerPlugin(http.ServerConfig{Port: 8080})
apiPlugin := api.New(server)
spec := openapi.NewPlugin(openapi.PluginOptions{
    Title:   "My API",
    Version: "1.0.0",
}).From(apiPlugin)

a := app.New("my-service")
a.Use(server)
a.Use(apiPlugin)
a.Use(spec) // serves GET /_/openapi.json
```

## Surface

- `openapi.NewPlugin(openapi.PluginOptions{Title, Version, Description, Servers, Route})` — constructor
- `openapi.Plugin.From(*api.Plugin)` — wires auto-discovery from typed `api.Endpoint` definitions; refreshed after all configurers for serve/describe
- `openapi.Plugin.AddRoute(DiscoveredRoute)` — explicit route registration for low-level callers
- `openapi.Plugin.Spec()` — accessor for the rendered `*Document`
- `openapi.Plugin.OpenAPISpecJSON()` — returns the same canonical bytes used by
  the runtime handler and describe artifacts
- `openapi.Plugin.RegisterOn(server)` — registers the spec-serving HTTP route (default `GET /_/openapi.json`)
- `openapi.Plugin.Describe(*app.DescribeContext)` — implements `app.Describer`; writes the spec for build-time codegen
- `openapi.GenerateSpec([]DiscoveredRoute, Options) *Document` — pure generator (no plugin)
- `openapi.Document.JSON()` — canonical indented JSON with stable object keys
  and one trailing newline
- `openapi.DefaultDescribePath` (`schema/openapi.json`) and `openapi.DescribeGzPath` (`schema/openapi.json.gz`)

## Auto-Generated Schemas

Schemas are derived from Go structs declared on typed `api.Endpoint` builders:

```go
api.Endpoint("POST", "/users").
    Body(api.Type[CreateUserBody]()).      // request schema
    ReturnsStatus(201, "Created", api.Type[User]()). // primary response
    Response(202, "Accepted", nil).        // additional response
    MayThrow(errors.CodeConflict).
    Throws(409, "Already exists", api.Type[Error]()).
    Handle(handler)
```

`MayThrowDetails(code, api.Type[T]())` publishes `T` as the declared error's
`x-putnami-client` schema — the `details` member, never the envelope (ADR 0006)
— and documents `details` on the error response for that status: `T`, or
`anyOf` the declared types in error-code order when several codes share the
status. `MayThrow` and `Throws` publish no details schema.

Struct tags surfaced in the spec:

- `json:"name"` → property name (overrides Go field name)
- `validate:"required"` → adds field to `required`
- `validate:"uuid"` / `validate:"email"` → sets `format`; `validate:"url"` → `format: uri`
- `validate:"min=N"` / `max=N` → `minimum` / `maximum`; `minlen=N` / `maxlen=N` → `minLength` / `maxLength`
- `validate:"pattern=RE"` → `pattern`; `validate:"oneof=a|b|c"` → `enum`
- `default:"..."` → a typed OpenAPI default; invalid values fail document serialization
- `description:"..."` → property/parameter description

`time.Time` → `{string, date-time}`, `[]byte` → `{string, byte}`. A value the
provider does not interpret is declared, never left as the empty schema:
`api.BinaryStream(maxBytes)` publishes a `*/*` media entry with `type: string`,
`format: binary`, `x-putnami-streamed: true`, and the positive
`x-putnami-max-bytes` transfer bound.
The sender supplies the concrete Content-Type at runtime. This remains unary
REST and never advertises Connect. Bounded `api.Binary(type, maxBytes)` keeps
its fixed media entry and positive byte-bound extension.

`json.RawMessage` and `any` → `{"x-putnami-json": "any"}` (a pointer adds no
`nullable`: null is already one of its values), and `map[string]any` or
`map[string]json.RawMessage` → `{type: object, additionalProperties: true}`.
Generated clients carry them as `json.RawMessage` / `unknown`. The error
envelope's `details` member uses the same declaration, so no error response
publishes `{}`. See `protocols/clientcontract/doc/adr/0008-opaque-json-is-a-declaration.md`. Self-referential
types are emitted under `components.schemas` and referenced via `$ref`.
Struct field selection matches `encoding/json`, including anonymous promotion,
dominance, ambiguity omission, and exact Go-name fallback for absent or invalid
JSON tag names.

`Returns` retains the HTTP 200 default. On unary endpoints, `ReturnsStatus`
supplies another primary 2xx status and description; `Response` adds other
success responses.

## Streaming endpoints

Endpoints declared with `api.Stream` / `api.StreamOf[T]()` are documented with a
description noting the stream mode (server / client / bidirectional) and the
supported transports (SSE for server streams, WebSocket for any mode). The
`DiscoveredRoute.StreamMode` is propagated to the operation so consumers can
detect streaming endpoints.

## The first-party client contract

When the api plugin declares a client service (`api.WithClientService`), the
document carries `x-putnami-client` on itself and on every operation. That
contract is what generated clients read, so the plugin publishes only what this
provider can actually honor and fails document generation otherwise, naming the
route:

- **Transports.** A server stream publishes SSE. WebSocket is not published: the
  framework's HTTP server negotiates no `putnami.service.v1` subprotocol and
  refuses continuation frames. A client or bidirectional stream therefore has no
  transport left and is refused until the WebSocket server lands.
- **Connect.** A unary route publishes a `connect` transport after its own REST
  URL when — and only when — the api plugin carries both a protobuf route binding
  (`ClientProtobufMethods()`, published by the proto plugin) and the encodings of
  a mounted bridge (`ClientConnectEncodings()`). The published path is the
  protobuf method identity, because a Connect URL is the method. A declared
  stream publishes no Connect transport: the bridge answers a Connect URL with a
  single unary POST.
- **Integers.** Every integer carries a `format` from `int32 | int64 | uint32 |
  uint64` plus explicit `minimum` / `maximum`, exact decimal, narrowed by any
  author bound. The width is declared, never inferred from the transport.
- **Request bodies.** A `GET` or `HEAD` request body is refused: RFC 9110 leaves
  the payload undefined, so a generated client would send bytes the endpoint
  pipeline never reads.
- **Security.** A route with no rule publishes one anonymous alternative, and
  declared credential alternatives on it are refused. A route whose rule
  requires authentication publishes its declared alternatives, or derives one
  from a single credential profile; an anonymous alternative is refused. A rule
  that reports `OptionalAuthentication()` true (`security.Options{Optional:
  true}`) publishes its credential alternatives first and the anonymous
  alternative last, and the plain OpenAPI operation adds the empty requirement
  `{}`. See `go/framework/security/doc/adr/0002-an-optional-rule-serves-a-caller-that-presents-no-credential.md`.
- **External operations.** A route declared with
  `Client(api.ClientOperationOptions{External: "OCI Distribution Specification v1.1"})`
  carries `x-putnami-external-contract` with that authority and no
  `x-putnami-client`. Its own parameters, body and responses are documented as
  a provider without a contract documents them (the standard error body, no
  closed objects); component schemas stay first-party because the whole
  document shares them. Every first-party reader skips the operation. A blank
  authority, `External` combined with another client option, or `External` in
  a document without a client contract fails generation with the route named.
  See `protocols/clientcontract/doc/adr/0011-an-external-authority-can-own-an-operation.md`.

## Build-time generation (Describer)

`openapi.Plugin` implements `app.Describer`. When the application runs in
describe mode (`PUTNAMI_DESCRIBE` env set), the plugin writes:

- `<OutputDir>/schema/openapi.json` — canonical indented JSON spec
- `<OutputDir>/schema/openapi.json.gz` — gzipped sibling for runtime serving

The Putnami Go extension's `describe` phase compiles a host binary, runs it with
`PUTNAMI_DESCRIBE=all PUTNAMI_DESCRIBE_OUT=<.gen>`, and (unless
`options.generate.schema=false` in `putnami.json`) copies `schema/openapi.json`
into the project tree. The `.gz` companion stays under `.gen/`.

The build-time write does not require running the app: the configure phase runs
through, Describe refreshes the spec from the final route set, writes the files,
and the binary exits.
For projects that also expose direct `go.putnami.dev/http` routes, the Go
extension merges the static generate-phase OpenAPI document with the runtime
describe document by path and method. If a project has multiple `cmd/*`
binaries, configure `options["@putnami/go"].describe.entrypoint` so describe
runs the application binary rather than a utility binary.

See `doc/getting-started.md` for full reference.

## Maintained contract

This public package is `stable` in the workspace [support
catalog](../../../putnami.support.json). It renders one half of the
[`go/api-contracts`](../api/putnami.features.json) feature, whose
[specification](../api/specs/api-contracts.json) is owned by
[`go.putnami.dev/api`](../api); this package's own durable decision is the
[final-route-set ADR](doc/adr/0001-publish-the-final-route-set.md). Before v1.0,
follow the workspace [migration-based compatibility
policy](../../../RELEASE.md); do not infer strict compatibility between every
`0.x` minor.
