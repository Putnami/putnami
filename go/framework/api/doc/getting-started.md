# API Endpoints

`go.putnami.dev/api` is the transport-agnostic endpoint-builder module. You declare
an endpoint's validation, response shape, security, and middleware **once** with a
fluent builder; the api plugin dispatches it onto a transport (today
`*http.ServerPlugin`) and exposes route metadata for the OpenAPI, proto, gRPC, and
typed-client generators to consume.

It is the layer the other generator modules build on: `http` provides the runtime
primitives (`Context`, `Response`, `Handler`, `Middleware`), and `api` owns the
definition surface that those generators read.

## Streamed archive bodies

Use `Body(api.BinaryStream(maxBytes))` or `Returns(api.BinaryStream(maxBytes))` when the sender
chooses the media type and the payload must remain a stream. Read uploads with
`api.BinaryStreamBody(ctx)`; return downloads with
`api.BinaryStreamResponse(200, storedMediaType, reader)`. The latter transfers
ownership of a closable reader to the response writer. A concrete Content-Type
is required, including for empty payloads, and its parameters are preserved.

The generated Go upload input has `ContentType string` and `Body io.Reader`;
downloads return `Body io.ReadCloser`, which the caller closes after reading.
These are unary HTTP requests carrying octets unchanged. They do not use JSON,
base64, automatic upload retries, or a response cache. The declared bound is
enforced on this route while `ServerConfig.MaxBodySize` continues to protect
unrelated JSON endpoints. Streaming removes the whole-body allocation, not the
transfer bound. Keep `api.Binary(type, maxBytes)`
for bounded buffers with a fixed media type.

## Installation

```go
import (
    "go.putnami.dev/api"
    "go.putnami.dev/app"
    "go.putnami.dev/http"
)
```

## Quick start

A complete, runnable service: validate input, return a typed result, and wire the
plugin into the application lifecycle.

```go
package main

import (
    "go.putnami.dev/api"
    "go.putnami.dev/app"
    "go.putnami.dev/http"
)

type ListUsersQuery struct {
    Limit int `json:"limit" validate:"min=1,max=100"`
}

type CreateUserInput struct {
    Name  string `json:"name"  validate:"required,minlen=2"`
    Email string `json:"email" validate:"required,email"`
}

type User struct {
    ID   string `json:"id"`
    Name string `json:"name"`
}

func main() {
    server := http.NewServerPlugin(http.ServerConfig{Port: 8080})
    apiPlugin := api.New(server, api.WithPrefix("/v1"))

    apiPlugin.Register(api.Endpoint("GET", "/users").
        Description("List users").
        Query(api.Type[ListUsersQuery]()).
        Returns(api.Type[[]User]()).
        Throws(401, "Unauthorized", nil).
        Handle(func(ctx *http.EndpointContext) *http.Response {
            q, _ := http.QueryAs[ListUsersQuery](ctx)
            return http.JSON(listUsers(q.Limit))
        }))

    apiPlugin.Register(api.Endpoint("POST", "/users").
        Description("Create a user").
        Body(api.Type[CreateUserInput]()).
        Returns(api.Type[User]()).
        Handle(func(ctx *http.EndpointContext) *http.Response {
            in, _ := http.BodyAs[CreateUserInput](ctx)
            return http.JSONStatus(201, createUser(in))
        }))

    app.New("users-api").Use(server).Use(apiPlugin).ListenAndServe()
}
```

Register the `http.ServerPlugin` **before** the `api.Plugin` so the server is
configured first. `api.New(server, ...)` accepts any `api.Server` — `*http.ServerPlugin`
satisfies it today.

## The endpoint builder

`api.Endpoint(method, path)` returns an `*EndpointBuilder`. Chain the declarations
you need, then finalize with `Handle`, `HandleRaw`, or `Document`:

| Method | Purpose |
| ------ | ------- |
| `.Description(text)` | Human-readable summary (used by generators) |
| `.Params(api.Type[T]())` | Validate path parameters against `T`'s `validate:` tags |
| `.Query(api.Type[T]())` | Validate query parameters |
| `.Body(api.Type[T]())` | Validate the request body |
| `.Returns(api.Type[T]())` | Declare the 200 response schema |
| `.Response(status, desc, schema)` | Declare an additional success response |
| `.MayThrow(errors.Code...)` | Declare framework-known error codes for OpenAPI |
| `.MayThrowDetails(code, api.Type[T]())` | Declare an error code and the type of its `details` body |
| `.Throws(status, desc, schema)` / `.ThrowsAll(specs...)` | Declare error responses |
| `.Inject(name, token)` | Resolve a DI dependency into `ctx.Injected[name]` |
| `.Secure(rule)` | Apply an authorization rule (see `go.putnami.dev/security`) |
| `.Use(mw)` | Attach per-endpoint middleware |
| `.Cors(opts)` / `.RateLimit(opts)` / `.Cache(opts)` | Per-endpoint cross-cutting options |
| `.Handle(fn)` | Finalize with the validating pipeline |
| `.HandleRaw(fn)` | Finalize with a raw `http.Handler` (no validation) |
| `.Document()` | Record metadata only — no transport handler is registered |

`api.Type[T]()` is shorthand for `reflect.TypeFor[T]()`, used to pass a schema type
to the chained declarations.

## Request validation

Validation is declarative: annotate the schema struct with `validate:` tags (enforced
by `go.putnami.dev/schema`) and declare it on the builder. A request that fails
validation is rejected with `400` **before** your handler runs, so the handler only
ever sees valid input.

```go
type CreateUserInput struct {
    Name  string `json:"name"  validate:"required,minlen=2"`
    Email string `json:"email" validate:"required,email"`
}

api.Endpoint("POST", "/users").
    Body(api.Type[CreateUserInput]()).
    Handle(func(ctx *http.EndpointContext) *http.Response { /* ... */ })
```

Common constraints: `required`, `minlen=N` / `maxlen=N`, `min=N` / `max=N`, `email`,
`uuid`, `url`, `pattern=REGEX`, `oneof=a|b|c`. See the `schema` module for the full set.

### Reading validated input

Inside the handler, read validated values with the typed helpers:

```go
in, err := http.BodyAs[CreateUserInput](ctx)   // request body
q,  err := http.QueryAs[ListUsersQuery](ctx)    // query parameters
p,  err := http.ParamsAs[UserParams](ctx)       // path parameters
```

For a declared first-party body, the framework has already decoded the original
JSON directly into the declared type and validated it before dispatch. Required
means the property was present, so `false`, `0`, `""`, `[]`, and `{}` are kept;
missing and explicit `null` stay distinct. Nested structs, slice items, and map
values are checked recursively, root primitives and arrays are supported, exact
64-bit integers do not pass through `float64`, and optional defaults reach the
typed value returned by `BodyAs`.

`ctx.ValidatedBody` contains the validated top-level fields for object bodies.
Query and path inputs retain the lower-level validated maps exposed as
`ctx.ValidatedQuery` and `ctx.ValidatedParams`.

## Responses

Declare the response shapes so generators emit an accurate spec:

```go
api.Endpoint("GET", "/users/{id}").
    Params(api.Type[UserParams]()).
    Returns(api.Type[User]()).               // 200
    Response(204, "No content", nil).        // additional success
    Throws(404, "User not found", nil).      // error
    ThrowsAll(
        api.ResponseMeta{Status: 401, Description: "Unauthorized"},
        api.ResponseMeta{Status: 403, Description: "Forbidden"},
    ).
    Handle(getUser)
```

## Dependency injection

Resolve container singletons or scoped services into the handler with `.Inject`,
then read them back by name:

```go
apiPlugin.Register(api.Endpoint("GET", "/me").
    Inject("users", inject.TokenOf[*UserService]()).
    Handle(func(ctx *http.EndpointContext) *http.Response {
        svc, _ := http.InjectedAs[*UserService](ctx, "users")
        return http.JSON(svc.Current(ctx))
    }))
```

## Security and middleware

```go
api.Endpoint("DELETE", "/users/{id}").
    Secure(security.Options{Roles: []string{"admin"}}).
    Use(audit.Middleware()).
    Cors(http.CORSOptions{AllowOrigins: []string{"https://app.example.com"}}).
    RateLimit(http.RateLimitOptions{Max: 100, WindowMs: 60000}).
    Handle(deleteUser)
```

Security rules come from `go.putnami.dev/security` (`security.Options`,
`security.Guard`); the declared roles/scopes also surface in the generated OpenAPI spec.

## Streaming endpoints

Mark a request or response schema as a stream with `api.StreamOf[T]()` and wrap the
handler in the matching adapter. Stream endpoints must use `GET`.

| Mode | Adapter | Transport |
| ---- | ------- | --------- |
| Server (push) | `api.ServerStream(fn)` | SSE (default) or WebSocket |
| Client (upload) | `api.ClientStream(fn)` | WebSocket |
| Bidirectional | `api.BidiStream(fn)` | WebSocket |

```go
// Server stream: push events to the client.
api.Endpoint("GET", "/feed").
    Returns(api.StreamOf[Event]()).
    Handle(api.ServerStream(func(ctx *api.ServerStreamContext[Event]) error {
        for ev := range source() {
            if err := ctx.Send(ev); err != nil {
                return err
            }
        }
        return nil
    }))

// Bidirectional stream: request/response over one WebSocket.
api.Endpoint("GET", "/chat").
    Body(api.StreamOf[ClientMsg]()).
    Returns(api.StreamOf[ServerMsg]()).
    Handle(api.BidiStream(func(ctx *api.BidiStreamContext[ClientMsg, ServerMsg]) error {
        for msg := range ctx.Messages() {
            if err := ctx.Send(reply(msg)); err != nil {
                return err
            }
        }
        return ctx.Err()
    }))
```

The stream mode is captured on `DiscoveredRoute.StreamMode` so downstream generators
emit the correct shape (proto, for example, prefixes the Request/Reply with `stream`).

A client stream declares both types: `api.ClientStream[TIn, TOut]`. The handler
returns the single value its caller waits for with `ctx.Result(v)`.

```go
api.Endpoint("GET", "/upload").
    Body(api.StreamOf[Chunk]()).
    Returns(api.Type[UploadSummary]()).
    Handle(api.ClientStream(func(ctx *api.ClientStreamContext[Chunk, UploadSummary]) error {
        total := 0
        for range ctx.Messages() {
            total++
        }
        if err := ctx.Err(); err != nil {
            return err
        }
        ctx.Result(UploadSummary{Count: total})
        return nil
    }))
```

### First-party WebSocket streams

An API declared with `api.WithClientService(...)` serves its stream routes over
the published first-party WebSocket protocol. A generated client puts no
credential on the HTTP upgrade — identity, credentials, deadline, declared
headers and tracing context arrive in the protocol's first frame — so the
framework rebuilds the request from that frame and then runs **your** endpoint's
security rule, middleware and validation on it before answering `ready`. Nothing
reaches the handler ahead of that, and a refusal reaches the caller as a typed
error carrying the same status and code as a unary call.

Declare the bounds you want under `resilience.stream` on the operation's client
options: `handshakeTimeoutMs`, `idleTimeoutMs`, `heartbeatMs`, `maxFrameBytes`
and `maxBufferedMessages`. A heartbeat you do not declare is not sent.
`.Inject()` is not supported on stream endpoints — capture dependencies via closure or
resolve them inside the handler.

## Path syntax

| Token | Meaning |
| ----- | ------- |
| `/users` | exact match |
| `/users/{id}` | single-segment parameter — rejects slashes |
| `/{name...}/...` | catch-all — captures one or more slash-separated segments, position-agnostic |

A catch-all binds the joined multi-segment value (e.g.
`go.putnami.dev/protocol/diagnostic`) to the named param, and may appear before a
fixed suffix or another segment (`/{module...}/-/blobs/upload`). Only one catch-all
per path is supported; the framework routes but does not validate segment shape.

## The plugin lifecycle

```go
apiPlugin := api.New(server, api.WithPrefix("/v1"))
apiPlugin.Register(endpointDefinition)   // queue an endpoint
```

`WithPrefix` normalizes its value with the platform protocol's prefix rule
(`protocol/platform.NormalizePrefix`): whitespace and slashes at either end are dropped,
one leading slash is added, and `""` or `"/"` mounts at the root. `"v1"`, `" v1"`,
`"//v1"` and `"/v1/"` all mount under `/v1`.

1. You build endpoints with `api.Endpoint(...).Handle(...)` and `Register` them.
2. `app.Start()` runs `apiPlugin.Configure()`, which builds each pending endpoint into
   an `http.Handler`, registers it on the transport via `Server.Handle`, and records a
   `DiscoveredRoute`.
3. Downstream plugins (openapi, proto, gRPC, typed clients) read
   `apiPlugin.DiscoveredRoutes()` in their own `Configure()`.

So registering the OpenAPI plugin after the api plugin is all it takes to publish a spec:

```go
openapiPlugin := openapi.NewPlugin(openapi.PluginOptions{Title: "Users API"}).From(apiPlugin)
app.New("users-api").Use(server).Use(apiPlugin).Use(openapiPlugin).ListenAndServe()
```

## Documentation-only endpoints

When a route is already mounted elsewhere (e.g. you called `server.GET` directly) but
you still want it in the generated spec, finalize with `.Document()` instead of
`Handle`: the api plugin records the metadata without registering a transport handler.

```go
apiPlugin.Register(api.Endpoint("GET", "/.well-known/putnami/events").
    Returns(api.Type[Manifest]()).
    Document())
server.GET("/.well-known/putnami/events", manifestHandler)
```

## See also

- `go.putnami.dev/http` — runtime `Context`, `Response`, middleware, and the server.
- `go.putnami.dev/schema` — the `validate:` constraint language.
- `go.putnami.dev/openapi` / `go.putnami.dev/proto` — generators that consume
  `DiscoveredRoutes()`.

## Contract and compatibility

See the [API contracts specification](../specs/api-contracts.json), the
[one-declaration ADR](adr/0001-one-declaration-many-consumers.md), the
[producer-lineage ADR](adr/0002-generated-clients-carry-producer-lineage.md), and
[support evidence](../README.md#support-and-contract). The package is stable and
maintained, with the workspace's [pre-1.0 migration
policy](../../../../RELEASE.md), not strict compatibility between every `0.x`
minor.
