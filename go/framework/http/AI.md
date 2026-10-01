# go.putnami.dev/http

HTTP server, trie-based router, middleware chain, endpoint builder, and response helpers.

## Quick Start

```go
import (
    "go.putnami.dev/app"
    "go.putnami.dev/http"
)

server := http.NewServerPlugin(http.ServerConfig{Port: 8080})

server.GET("/users", listUsers)
server.POST("/users", createUser)
server.GET("/users/{id}", getUser)

server.Use(http.Recovery())
server.Use(http.RequestID())
server.Use(http.Logging(http.LoggerOptions{Exclude: []string{"/healthz", "/livez", "/readyz"}}))

platformPlugin := platform.NewPlugin(platform.Config{})
platformPlugin.RegisterOn(server)

a := app.New("my-service")
a.Use(server)
a.Use(platformPlugin) // GET /healthz /livez /readyz /version
a.ListenAndServe()
```

> Prefer `go.putnami.dev/platform` for new code — it mounts `/healthz`, `/livez`, `/readyz`, `/version`, and (opt-in) `/debug/pprof/*` and auto-discovers `app.HealthChecker` / `app.ReadinessChecker` probes. `http.NewHealthPlugin()` is still supported (it now auto-discovers `HealthChecker` too) and exposes `/_/health`, but it covers only liveness.

## Routing

| Pattern   | Example                              | Description                                                                                              |
| --------- | ------------------------------------ | -------------------------------------------------------------------------------------------------------- |
| Static    | `/users`                             | Exact match                                                                                              |
| Parameter | `/users/{id}`                        | Single-segment, no slashes; captured in `ctx.Param("id")`                                                |
| Catch-all | `/files/{path...}` or `/{m...}/-/up` | One or more slash-separated segments (joined); position-agnostic; captured in `ctx.Param("path"/"m")`    |
| Catch-all | `/files/{path...}`                   | Named catch-all; captured in `ctx.Param("path")`                                                          |

Methods: `GET`, `POST`, `PUT`, `PATCH`, `DELETE`, or `server.Route("OPTIONS", path, handler)`.

## Handler Context

```go
func handler(ctx *http.Context) *http.Response {
    id := ctx.Param("id")              // path parameter
    q := ctx.Query("search")           // query parameter
    auth := ctx.Header("Authorization")

    var body CreateInput
    if err := ctx.Body(&body); err != nil {
        return http.JSONStatus(400, map[string]string{"error": err.Error()})
    }

    return http.JSON(result)
}
```

## Response Helpers

```go
http.JSON(data)                // 200 + JSON
http.JSONStatus(201, data)     // custom status + JSON
http.Text("hello")             // 200 + text/plain
http.NoContent()               // 204
http.Redirect("/login", 302)   // redirect
http.NotFound()                // 404
http.Unauthorized()            // 401
http.Forbidden()               // 403
http.InternalError(err.Error()) // 500 — takes a string message
```

## Middleware

```go
func MyMiddleware(ctx *http.Context, next func() *http.Response) *http.Response {
    // pre-processing
    resp := next()
    // post-processing (or return early to short-circuit)
    return resp
}

server.Use(MyMiddleware)
```

**Built-in:** `Recovery()`, `RequestID()`, `Logging()`, `RateLimit()`, `Compression()`.

## Handler Injection

`http.Inject` wraps a function with DI-resolved parameters. Singleton deps are
resolved once at startup (zero per-request cost). Scoped deps (registered via
`ProvideScopedFunc`) are resolved per-request from the DI scope automatically:

```go
server.GET("/users", http.Inject(func(svc *UserService, ctx *http.Context) *http.Response {
    return http.JSON(svc.ListAll())
}))

server.POST("/users", http.Inject(func(svc *UserService, ctx *http.Context) *http.Response {
    var body CreateInput
    if err := ctx.Body(&body); err != nil {
        return http.JSONStatus(400, map[string]string{"error": err.Error()})
    }
    return http.JSONStatus(201, svc.Create(body))
}))
```

`*Context` can appear at any position. All other parameters are resolved from DI.

Scoped deps work transparently — same handler signature, resolved per-request:

```go
// Register a scoped provider (new instance per HTTP request)
a.ProvideScopedFunc(NewRequestLogger)

server.GET("/users", http.Inject(func(log *RequestLogger, svc *UserService, ctx *http.Context) *http.Response {
    log.Info("listing users")     // log is unique per request
    return http.JSON(svc.ListAll()) // svc is a shared singleton
}))
```

## Streaming (SSE / WebSocket)

The server plugin implements server-, client-, and bidirectional streams over
a single endpoint. The transport is auto-negotiated from the request:

| Header | Transport |
| ------ | --------- |
| `Upgrade: websocket` | WebSocket (any stream mode) |
| `Accept: text/event-stream` (server stream only) | SSE |
| Otherwise | `400 Bad Request` |

`StreamMode` constants — `StreamModeServer`, `StreamModeClient`,
`StreamModeBidirectional` — describe the shape; `StreamHandler` is the low-level
contract. Most users go through the typed `api.Endpoint` builder
(`api.ServerStream`, `api.ClientStream`, `api.BidiStream`) rather than wiring
streams on `*ServerPlugin` directly.

The WebSocket implementation is RFC 6455-compliant (zero external deps) and
lives at `websocket.go`. SSE serving is at `stream.go::serveSSE`.

### Subprotocol negotiation

A route may declare the single `Sec-WebSocket-Protocol` token it speaks:

```go
plugin.HandleStream("/chat", http.StreamHandler{
    Mode:        http.StreamModeBidirectional,
    Subprotocol: "example.chat.v1",
    Serve: func(ctx *http.Context, conn *http.WebSocketConn) error {
        opcode, message, err := conn.ReadMessage() // continuations reassembled
        ...
        return conn.CloseWith(http.WebSocketCloseNormal, "done")
    },
})
```

When the client offers exactly that token, the server echoes it and calls
`Serve` with the framed connection instead of running the raw transport stream.
`Before` is **not** run on the upgrade in that case: a negotiated protocol
carries its admission material in its own first frame, so the route's security
and validation chain runs from `Serve`, once those headers are reconstructed.
A client that offers a token the route cannot speak is refused with 400; a
client that offers none keeps the raw stream and the ordinary `Before` order.
`go.putnami.dev/api` uses this seam for the first-party service protocol.

A wire that carries no admission material of its own sets `AdmitOnUpgrade:
true`. `Before` then runs on the upgrade request itself and refuses with an
ordinary HTTP response; the route speaks exactly its declared `Subprotocol`, or
no token when it is empty (any other offer is 400); and every admitted socket
reaches `Serve` — there is no raw stream and no SSE. `WriteBinary` sends a
binary message under the same bound as `WriteMessage`. `go.putnami.dev/api`
uses this for provider-owned wires (`api.ByteStream`, `.Subprotocol`).

`*WebSocketConn` owns RFC 6455 and nothing above it: masking, continuation
reassembly under `DefaultMaxBodySize` (`SetMaxMessageBytes` may lower the bound,
never raise it), ping answering, the reserved-bit / reserved-opcode /
unmasked-client-frame / oversized-control-frame refusals, UTF-8 validity of text
messages, and the `WebSocketClose*` codes. `Draining()` closes when the server
begins a graceful stop, so a long-lived conversation ends deliberately instead
of being cut mid-frame. A close reason is clear text on the wire: pass fixed
framework text, never payload or credential material.

## Endpoint Builder

The fluent endpoint builder (validation, DI injection, security, OpenAPI
metadata) lives in **`go.putnami.dev/api`**, not in this package — there is no
`http.Endpoint`. This package supplies the lower-level pieces the builder binds
to: `http.EndpointContext`, the typed extractors `http.ParamsAs` /
`http.QueryAs` / `http.BodyAs` / `http.InjectedAs`, and `http.SecurityRule`.

```go
import (
    "go.putnami.dev/api"
    "go.putnami.dev/http"
)

httpServer := http.NewServerPlugin(http.ServerConfig{})
apiPlugin := api.New(httpServer)

apiPlugin.Register(api.Endpoint("POST", "/users").
    Body(api.Type[CreateUserBody]()).
    Secure(&security.Options{Roles: []string{"admin"}}).
    Handle(http.Inject(func(svc *UserService, ctx *http.EndpointContext) *http.Response {
        body, err := http.BodyAs[CreateUserBody](ctx)
        if err != nil {
            return http.JSONStatus(400, map[string]string{"error": err.Error()})
        }
        return http.JSONStatus(201, svc.Create(body))
    })))

app.New("myapp").Use(httpServer).Use(apiPlugin).ListenAndServe()
```

See `go.putnami.dev/api` (and its `AI.md`) for the full builder reference.

## Maintained contract

This public package is `stable` in the workspace [support
catalog](../../../putnami.support.json). Its behavior is defined by the [HTTP
services specification](specs/http-services.json), the [request-scope
ADR](doc/adr/0001-request-scope-follows-the-response.md), and the [bounded-resource
ADR](doc/adr/0002-framework-owned-bounds.md). Before v1.0, follow the workspace
[migration-based compatibility policy](../../../RELEASE.md); do not infer strict
compatibility between every `0.x` minor.
