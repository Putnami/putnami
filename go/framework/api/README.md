# go.putnami.dev/api

Transport-agnostic endpoint definitions for the Putnami Go framework. Mirrors the TypeScript
`@putnami/application/api` surface: declare validation, response shape, security, and
middleware once; the api plugin dispatches the endpoint onto a transport (today
`*http.ServerPlugin`) and exposes route metadata for OpenAPI / proto / gRPC / typed-client
generators to consume.

## Usage

```go
import (
    "go.putnami.dev/api"
    "go.putnami.dev/app"
    "go.putnami.dev/errors"
    "go.putnami.dev/http"
    clientcontract "go.putnami.dev/protocol/clientcontract"
)

type ListUsersQuery struct {
    Limit int `json:"limit" validate:"min=1,max=100"`
}

type User struct {
    ID   string `json:"id"`
    Name string `json:"name"`
}

func main() {
    httpServer := http.NewServerPlugin(http.ServerConfig{Port: 8080})
    apiPlugin := api.New(httpServer, api.WithPrefix("/v1"))

    apiPlugin.Register(api.Endpoint("GET", "/users").
        Description("List users").
        Query(api.Type[ListUsersQuery]()).
        Returns(api.Type[[]User]()).
        MayThrow(errors.CodeUnauthorized).
        Throws(401, "Unauthorized", nil). // optional override for a custom description/schema
        Handle(listUsers))

    apiPlugin.Register(api.Endpoint("POST", "/users").
        ReturnsStatus(201, "Created", api.Type[User]()).
        Handle(createUser))

    app.New("myapp").Use(httpServer).Use(apiPlugin).ListenAndServe()
}
```

`Returns(type)` keeps HTTP 200 as the primary success response.
`ReturnsStatus(status, description, type)` declares another 2xx primary response
for a unary endpoint without adding a synthetic 200 to OpenAPI. Generated clients
retain its response type. Use `Response(...)` for additional success responses.
This metadata describes the contract; the handler's `*http.Response` still
determines the status sent on the wire.

## Path syntax

Three token shapes are recognised in the path argument to `api.Endpoint`:

```go
api.Endpoint("GET",  "/users/{id}")                   // single segment, no slashes
api.Endpoint("GET",  "/files/{path...}")              // catch-all — trailing
api.Endpoint("POST", "/{module...}/-/blobs/upload")   // catch-all — middle, before a literal suffix
api.Endpoint("GET",  "/{module...}/@v/{versionfile}") // catch-all + single-segment after
```

`{name}` captures exactly one segment and rejects slashes. `{name...}` captures one
or more slash-separated segments — the named parameter receives the joined value
(e.g. `module = "go.putnami.dev/protocol/diagnostic"`). Catch-all params may appear
anywhere in the pattern, not just at the end. Only one catch-all per path is
supported. The framework does not validate segment shape — the surface owns that.

OpenAPI 3.0 has no native multi-segment path-param syntax. The generator renders
catch-all params as plain `{name}` strings in the spec with a description noting
the multi-segment semantics.

## Typed client generation

`api.Clients(...)` is a build-time describer that generates a typed Go client from the
provider's OpenAPI spec — the single contract both the Go and TypeScript client emitters
share. Register it alongside the `openapi` plugin:

```go
apiPlugin := api.New(httpServer, api.WithClientService(api.ClientServiceOptions{
    Service: clientcontract.Service{ID: "items", Audience: "urn:putnami:items"},
}))
openapiPlugin := openapi.NewPlugin(openapi.PluginOptions{Title: "Items"}).From(apiPlugin)
clients := api.Clients(api.ClientsOptions{
    Targets: []string{"go", "ts"},
    Go: api.GoClientOptions{
        PackageName: "itemsclient",
        ClientName:  "ItemsClient",
    },
    TS: api.TSClientOptions{PackageName: "@example/items-client"},
}).From(apiPlugin)
app.New("svc").Use(httpServer).Use(apiPlugin).Use(openapiPlugin).Use(clients)
```

During `putnami build` the describer:

1. reads the canonical OpenAPI bytes in-memory from the `openapi` plugin
   (discovered via the `SpecSource` interface — an explicit dependency, no
   describe-ordering race); those are the same bytes the plugin serves and
   describes, so publication that only reapplies the canonical object-key and
   trailing-newline format preserves the generated client's `SpecHash` for
   workspace regeneration;
2. writes the client-generation contract to `.gen/clientgen/config.json`;
3. `putnami clientgen` synchronizes every configured language target and writes a
   validated ownership manifest beside each generated client.

The consumer registers `client.Services()` and the generated
`RegisterItemsClient` binding, then calls typed methods. URL, client identity,
credential sources and secrets come from typed runtime config; generated source
contains none of those deployment values.

First-party generation fails when a schema, transport, security rule, stream,
error or policy cannot be represented without loss. Optional, nullable and empty
values remain distinct in generated types.

### Routes an external authority owns

A provider can serve a standard protocol beside its own routes on one API: an
OCI registry, a Go module proxy, an npm registry. The standard legs belong to
the standard, so mark each one with the authority that owns it:

```go
apiPlugin.Register(api.Endpoint("GET", "/v2/{name}/manifests/{reference}").
    Client(api.ClientOperationOptions{External: "OCI Distribution Specification v1.1"}).
    HandleRaw(getManifest))
apiPlugin.Register(api.Endpoint("GET", "/v2/_putnami/capabilities").
    Returns(api.Type[Capabilities]()).
    HandleRaw(capabilities))
```

The marked route stays served and stays in the OpenAPI document, where it
carries `x-putnami-external-contract` instead of `x-putnami-client`. It has no
protobuf method, no Connect URL and no generated client method; its callers use
the standard's own adapters. It is served by the standard request pipeline: its
body is decoded leniently, a failure answers the standard error body, and a
stream keeps the raw transport. The other routes of the API stay first-party.

`External` stands alone. `Configure` fails, before any route is bound, when the
authority is blank, when `External` is combined with any other field of
`ClientOperationOptions`, or when the API has no `api.WithClientService(...)`.
ADR 0011 of `protocols/clientcontract` records the decision.

### The generated Go surface

Each operation emits one method on the client:

```go
func (c *ItemsClient) GetItems(ctx context.Context, in GetItemsInput) (*Item, error)
```

- The method name is the provider's declared `operationId` when the provider
  authored one. When the `operationId` is only this framework's synthesis from
  method and path, the name is the REST idiom — `CreateUsers` for `POST /users` —
  which is also the name the proto and gRPC bridge uses, so one operation keeps
  one Go symbol across transports.
- A declared success body is returned by pointer, so a caller distinguishes "no
  value" from a zero struct. An operation with no success body returns `error`.
- `in` groups the declared inputs under `Path`, `Query`, `Header` and `Body`.
- An operation that declares more than one success status returns
  `*<Method>Result`: `Status` is the declared status the provider answered, and
  the body that status declares is decoded under its own schema. When every
  status that carries a body declares the same schema, one `Body` field serves
  them all and is nil on a status that declares none — `200`/`201` "existing or
  created", `200`/`204` "a body or nothing". When the schemas differ, each status
  gets its own `Body<status>` field, non-nil only on that status. See [ADR
  0008](doc/adr/0008-an-operation-with-several-success-statuses-returns-the-status.md).

```go
res, err := c.DeployWorkspace(ctx, in) // declares 200 Deployment, 202 Job
switch res.Status {
case 200:
    use(res.Body200)
case 202:
    poll(res.Body202)
}
```

### What generation refuses

The first-party contract has no permissive fallback: anything no runtime can
honor fails while the client is generated, never at the first call.

| Declaration | Why it is refused |
| --- | --- |
| A stream shape this projection has no transport for | Falling back to a unary REST transport would publish a call the provider never serves |
| A WebSocket transport encoding `proto` | Wire v1 keeps the encoding in the contract and carries WebSocket messages as JSON |
| `{"type": "integer"}` with no `format` | The width would be inferred, and the two languages infer differently |
| `Throws(status)` with no matching `MayThrow(code)` | A declared error with no stable wire code collapses back to an untyped remote error |
| A request body on `GET` or `HEAD` | RFC 9110 leaves the payload undefined and intermediaries may drop it |
| Several success statuses dispatched on Connect | A Connect response carries one success status, so the caller could not tell the variants apart |
| Several success statuses beside a raw octet response | The single octet payload names no status |

`GoClientOptions.OmitOperations` names operations, by `operationId`, that the
Go target leaves out instead of failing the whole provider — the escape for an
operation the Go emitter cannot represent yet, such as one with typed response
headers. The default is unchanged: such an operation fails generation. Each
operation left out is named in the build output, in the generated client's doc
comment and in `omittedOperations` of its `client.putnami.json`, and workspace
coverage counts it as uncovered. A name the contract does not declare fails.
A TypeScript provider sets the same option as `go: { omitOperations }` on
`clientGenerator(...)`.

```go
api.Clients(api.ClientsOptions{
    Targets: []string{"ts", "go"},
    Go: api.GoClientOptions{PackageName: "identityclient", OmitOperations: []string{"listAuditEvents"}},
})
```

A `DELETE` body is representable and stays allowed. When the caller sends one it
is decoded and validated like any other body; when the caller sends none the
request still reaches the handler.

For declared JSON bodies, the provider decodes directly into the declared Go
type and validates against the original JSON bytes before the handler runs.
Required wire presence therefore preserves present `false`, `0`, empty strings,
arrays, and objects; explicit null, missing values, nested constraints, root
primitives/arrays, exact 64-bit integers, and optional defaults keep their
declared meaning.

### First-party WebSocket streams

An API declared with `api.WithClientService(...)` serves every stream route over
the published first-party WebSocket protocol as well as SSE, and the published
contract declares SSE first for a server stream — that order is the dispatch
order a generated client follows.

A generated client puts no credential on the HTTP upgrade. Identity, credentials,
deadline, budget, declared headers and propagation context arrive in the
protocol's mandatory first frame, so a browser and a Go client admit the same
way. The framework rebuilds the request from that frame — each declared
credential profile onto its injection header, ordinary headers replayed, tracing
context restored — and then runs the endpoint's own security rule, middleware and
parameter validation on it. Only then does the provider answer `ready`; nothing
reaches the handler ahead of it, and a refusal reaches the caller as a typed
error carrying the same status and code as a unary call. The conversation state
is re-read after that chain, so a slow credential check never admits a caller
that cancelled or whose deadline elapsed.

`resilience.stream` supplies the handshake, idle, heartbeat, frame and queue
bounds; a heartbeat that is not declared is not sent. `ADR 0004` records the
decision.

### Producer attribution

The contract's `design.operations` table attributes each endpoint to the feature
that produced it. Ownership is the module that owns the `api.Plugin` the endpoint
was registered on — never the client generator's module — so an endpoint whose
owner declares no feature is absent from the table and is generated
**unattributed** rather than inheriting a neighbouring feature. The generated
`<Client>Design` value therefore carries the producer project and feature per
operation, keyed by `CanonicalOperationID` (the same identity the OpenAPI spec
stamps as its `operationId` and the design graph's `generatedFrom` edges use).
The Go method symbol is derived separately, so a canonical id with punctuation
survives into the descriptor and every `FeatureTrace`.

The low-level entry points — `ReadOpenAPISpec` (OpenAPI JSON → `SpecIR`) and
`GenerateClientFromIR` (`SpecIR` → Go source) — are exported for tooling that wants to
drive generation directly. See `go/samples/service-to-service` for a worked example.

## Why a separate package?

Keeping the endpoint builder and orchestrator out of `http/` lets a future edge or
in-memory transport implement `api.Server` without forcing the HTTP server package to be
in scope. It also keeps `http/` focused on the runtime primitives — `Context`, `Response`,
`Handler`, `Middleware`, and the listener — and lets `api/` own the API definition layer.

## Support and contract

The SDD owner is `go`. `go.putnami.dev/api` is public, documented, maintained, and
classified `stable` in the workspace [support catalog](../../../putnami.support.json).
Before v1.0.0, a minor `0.x` release may still contain a breaking change; the
[release policy](../../../RELEASE.md) requires release notes and migration
documentation rather than strict compatibility between every pre-1.0 minor.

This package owns two user-facing features:

- [`go/api-contracts`](putnami.features.json) — declare an endpoint once, serve it,
  and publish the OpenAPI and Protobuf contracts that describe it. The
  [specification](specs/api-contracts.json) also covers
  [`go.putnami.dev/openapi`](../openapi) and [`go.putnami.dev/proto`](../proto),
  which render those contracts from this package's route metadata.
- [`go/typed-service-clients`](../client/putnami.features.json) — the client
  generator half. Its [specification](../client/specs/typed-service-clients.json)
  lives with [`go.putnami.dev/client`](../client), the runtime a generated client
  is built on.

Durable decisions: [one declaration, many consumers](doc/adr/0001-one-declaration-many-consumers.md),
[per-operation producer lineage](doc/adr/0002-generated-clients-carry-producer-lineage.md),
and [the generated Go symbol versus the operation
identity](doc/adr/0003-the-generated-go-symbol-is-idiomatic-the-operation-id-is-the-identity.md).

Regression evidence covers the [endpoint builder and validation
pipeline](endpoint_test.go), [transport dispatch, prefixes, and document-only
routes](plugin_test.go), [streaming endpoints](stream_test.go), [design
contribution](design_test.go), [client generation](clientgen_test.go), [spec
reading](clientir_test.go), [the generation contract](clients_test.go),
[standalone and in-module client projects](clientgen_project_test.go), and
[cross-language client sync](clientgen_crosslang_test.go).

## Caller-resolved page endpoints

First-party generated clients export `Bind<Client>(client.ServiceBinding)` and
`Close<Client>`. For a shared page operation, resolve the owner's origin,
audience and route at call time, bind once for that owner's page sequence, and
close the binding afterward. For example, a generated `PageClient` supports:

```go
pages, err := BindPageClient(client.ServiceBinding{
    URL: ownerOrigin,
    ClientID: "replica",
    OperationPaths: map[string]string{pageOperationID: ownerPath},
    Credentials: map[string]client.CredentialBinding{
        "owner": {Source: client.CredentialSourceGCPIDToken, Audience: ownerAudience},
    },
})
if err != nil { return err }
defer ClosePageClient(pages)
```

Use the operation ID from the published contract as the map key. A mapping
accepts only a fixed unary REST JSON operation and an unescaped same-authority
path. It retains the declared method, security, schemas and resilience. With
an origin URL, the mapped path is the owner's full endpoint path; an existing
URL prefix is retained. Owners declare the common envelope with
`type SnapshotPage clientcontract.Page[Rows]` and publish
`api.Type[SnapshotPage]()`. Use this named owner type so emitted clients have a
plain schema name. Pin the resolved query/response schemas with
`clientcontract.ValidatePageTransportSchemas`.
