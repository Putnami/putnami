# API Endpoints

`go.putnami.dev/api` is where a Go service declares what its API *is*. An
endpoint's method, path, input schemas, response shapes, declared errors,
security rule, middleware, and injected dependencies are written once. The api
plugin binds that declaration to a transport and hands the same metadata to every
consumer that needs it — the [OpenAPI document](/docs/frameworks/go/openapi), the
[Protobuf service](/docs/frameworks/go/grpc), the Connect bridge, and generated
typed clients.

There is no `http.Endpoint`. The [`http`](/docs/frameworks/go/http) package owns
the runtime primitives — `Context`, `Response`, `Handler`, `Middleware`, and the
listener — and `api` owns the definition layer.

## Declaring an endpoint

```go
import (
    "go.putnami.dev/api"
    "go.putnami.dev/app"
    "go.putnami.dev/errors"
    fhttp "go.putnami.dev/http"
)

type ListUsersQuery struct {
    Limit int `json:"limit" validate:"min=1,max=100"`
}

type User struct {
    ID   string `json:"id"`
    Name string `json:"name"`
}

server := fhttp.NewServerPlugin(fhttp.ServerConfig{Port: 8080})
apiPlugin := api.New(server, api.WithPrefix("/v1"))

apiPlugin.Register(api.Endpoint("GET", "/users").
    Description("List users").
    Query(api.Type[ListUsersQuery]()).
    Returns(api.Type[[]User]()).
    MayThrow(errors.CodeUnauthorized).
    Handle(listUsers))

app.New("my-service").Use(server).Use(apiPlugin).ListenAndServe()
```

`api.Type[T]()` turns a Go type into the schema descriptor the builder stores.
Constraints come from the `validate:` struct tags described in
[Validation](/docs/frameworks/go/validation).

## The validation pipeline

A bound endpoint validates in a fixed order before the handler runs:

1. **path parameters** — validated with coercion against the `Params` schema
2. **query parameters** — validated with coercion against the `Query` schema
3. **request body** — validated against the `Body` schema, for `POST`, `PUT`, and
   `PATCH` only, so a declared body schema never rejects a `GET` or `DELETE`
4. **injected dependencies** — resolved from the request's DI scope

Any validation failure answers `400` with the failing fields:

```json
{
  "error": "Bad Request",
  "message": "Validation failed",
  "details": [{ "field": "query.limit", "message": "must be at most 100", "constraint": "max" }]
}
```

A dependency-resolution failure answers a generic `500`; the real cause is
recorded on the request and logged, never returned.

## Handler shapes

```go
// Typed handler — validated params/query/body plus resolved dependencies
.Handle(func(ctx *fhttp.EndpointContext) *fhttp.Response { … })

// DI-injected handler
.Handle(fhttp.Inject(func(svc *UserService, ctx *fhttp.EndpointContext) *fhttp.Response { … }))

// Raw handler — no validation pipeline, no injected map
.HandleRaw(func(ctx *fhttp.Context) *fhttp.Response { … })

// Documentation only — publishes the schema, binds no handler
.Document()
```

Read the validated input with the typed extractors:

```go
body, err := fhttp.BodyAs[CreateUserInput](ctx)
params, err := fhttp.ParamsAs[UserParams](ctx)
query, err := fhttp.QueryAs[ListUsersQuery](ctx)
```

`Document()` is how you publish the schema for a route that is mounted
elsewhere — typically because the handler is owned outside the api builder:

```go
apiPlugin.Register(api.Endpoint("GET", "/.well-known/putnami/events").
    Returns(api.Type[Manifest]()).
    Document())
server.GET("/.well-known/putnami/events", manifestHandler)
```

Consumers split on whether they describe the route or map a handler:

| Consumer | Document-only endpoint |
|----------|------------------------|
| The HTTP transport | not bound — the directly mounted handler serves it |
| [OpenAPI](/docs/frameworks/go/openapi) | **included** — the route is genuinely served |
| Generated typed clients | **included**, for the same reason |
| [Protobuf service](/docs/frameworks/go/grpc) | skipped |
| [Connect bridge](/docs/frameworks/go/grpc) | skipped |

The last two bind an api-plugin handler that a document-only endpoint does not
have, so an emitted RPC would advertise a URL that answers nothing. This is the
one place where the two published contracts legitimately differ.

## Path syntax

```go
api.Endpoint("GET",  "/users/{id}")                   // one segment, no slashes
api.Endpoint("GET",  "/files/{path...}")              // catch-all, trailing
api.Endpoint("POST", "/{module...}/-/blobs/upload")   // catch-all before a literal suffix
```

`{name}` captures exactly one segment and rejects slashes. `{name...}` captures
one or more slash-separated segments and the parameter receives the joined value.
A catch-all may appear anywhere in the pattern; only one per path is supported.

OpenAPI 3.0 has no multi-segment path-parameter syntax, so the generator renders a
catch-all as a plain `{name}` string whose description states the multi-segment
semantics, and generated clients substitute the joined value without re-escaping
the separators.

## One identity across every artifact

Operation identity is derived from method and path by one canonical function. The
OpenAPI `operationId`, the Protobuf RPC name, the Connect bridge URL, and every
generated client descriptor all use it, so one operation can be followed across
all four. Duplicate or colliding routes are disambiguated deterministically rather
than overwriting one another.

Language symbol normalization happens on top of the canonical id and never
replaces it: a Go method named `GetV1_Operator_Cli_usage` still traces back to the
canonical `getV1_Operator_Cli-usage`.

## Generating typed clients

`api.Clients(...)` is a build-time describer that generates a typed client from
the provider's OpenAPI document — the single contract the Go and TypeScript
emitters share:

```go
openapiPlugin := openapi.NewPlugin(openapi.PluginOptions{Title: "Items"}).From(apiPlugin)
clients := api.Clients(api.ClientsOptions{
    Go: api.GoClientOptions{PackageName: "itemsclient", ClientName: "ItemsClient"},
}).From(apiPlugin)

app.New("svc").Use(server).Use(apiPlugin).Use(openapiPlugin).Use(clients)
```

During `putnami build` the describer reads the document in memory from the
openapi plugin — an explicit dependency, so there is no describe-ordering race —
writes the generation contract to `.gen/clientgen/config.json`, and generates the
client into `clients/go` by default.

Each generated operation records the project and feature that produced it, taken
from the module owning the api plugin. An operation whose owner declares no
feature stays **unattributed** rather than inheriting a neighbour's lineage. See
[Service Clients](/docs/frameworks/go/service-clients) for the runtime side.

## Registration is a wiring-time activity

Transport binding happens during the configure phase. `Definitions()` is readable
before it, for adapters that need the full builder metadata;
`DiscoveredRoutes()` is the flattened, post-binding view every contract consumer
reads. An endpoint registered after the configure phase is never served.

## Support and contract

`go.putnami.dev/api` is `stable` in the workspace support catalog. Its behavior is
defined by the API contracts specification and two accepted decision records next
to the package source (`go/framework/api/specs/` and `go/framework/api/doc/adr/`).
Before v1.0.0 a minor `0.x` release may still contain a documented breaking
change; strict compatibility between every pre-1.0 minor is not promised.

## Related guides

- [HTTP & Middleware](/docs/frameworks/go/http) — the runtime primitives endpoints bind to
- [Validation](/docs/frameworks/go/validation) — the `validate:` constraint language
- [OpenAPI](/docs/frameworks/go/openapi) — the published document
- [gRPC & Connect](/docs/frameworks/go/grpc) — the Protobuf service and the Connect bridge
- [Service Clients](/docs/frameworks/go/service-clients) — calling a generated client
