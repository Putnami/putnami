# HTTP Server

The `http` package provides the HTTP server, router, middleware chain, endpoint builder, content negotiation, and response helpers.

`Stream(status, mediaType, reader)` writes a raw response incrementally and owns
closing the source when it implements `io.Closer`. `BodyBytes` refuses that
response without reading it, and compression passes it through unchanged.
Scoped resources remain alive until copying finishes; transactions still finalize
before response headers. Request cancellation closes closable sources. A reader
that cannot be closed must itself respect the request context.

## Server Plugin

`ServerPlugin` is a plugin that serves HTTP requests:

```go
server := http.NewServerPlugin(http.ServerConfig{Port: 8080})

// Register routes
server.GET("/users", listUsers)
server.POST("/users", createUser)
server.GET("/users/{id}", getUser)
server.PUT("/users/{id}", updateUser)
server.DELETE("/users/{id}", deleteUser)

// Add global middleware
server.Use(http.Recovery())
server.Use(http.RequestID())
server.Use(http.Logging(http.LoggerOptions{
    Exclude: []string{"/_/health"},
}))

// Add to your application
a.Module.Use(server)
```

## Router

The router uses a trie-based structure supporting:
- **Exact segments**: `/users`
- **Parameters**: `/users/{id}` — single segment, no slashes; captured in `ctx.Param("id")`
- **Catch-all parameters**: `/files/{path...}` or `/{module...}/-/blobs/upload` —
  capture one or more slash-separated segments (the joined value); position-agnostic
  (trailing OR before a fixed suffix / another single-segment param)
- **Named catch-alls**: `/files/{path...}` — captured in `ctx.Param("path")`

```go
server.GET("/users/{id}/posts/{postId}", getPost)
// GET /users/42/posts/99 → ctx.Param("id") == "42", ctx.Param("postId") == "99"

server.POST("/{module...}/-/blobs/upload", uploadBlob)
// POST /go.putnami.dev/protocol/diagnostic/-/blobs/upload
//   → ctx.Param("module") == "go.putnami.dev/protocol/diagnostic"
```

## Context

Each request gets a `Context` with helpers:

```go
func handler(ctx *http.Context) *http.Response {
    id := ctx.Param("id")          // Path parameter
    q := ctx.Query("search")       // Query parameter
    auth := ctx.Header("Authorization")

    var body CreateUserInput
    if err := ctx.Body(&body); err != nil {
        return http.JSONStatus(400, map[string]string{"error": err.Error()})
    }

    if ctx.IsSecured() {
        user := ctx.User // *Claims with typed fields
    }

    return http.JSON(result)
}
```

## Responses

Response helpers return `*Response` structs:

```go
http.JSON(data)                    // 200 + JSON
http.JSONStatus(201, data)         // Custom status + JSON
http.Text("hello")                 // 200 + text/plain
http.Redirect("/login", 302)       // Redirect
http.NoContent()                   // 204
http.NotFound()                    // 404
http.Unauthorized()                // 401
http.Forbidden()                   // 403
http.InternalError(err)            // 500

// Add headers
resp.WithHeader("X-Custom", "value")

// Access serialized body bytes
bytes, err := resp.BodyBytes()
```

## Middleware

Middleware wraps handlers with pre/post logic:

```go
func MyMiddleware(ctx *http.Context, next func() *http.Response) *http.Response {
    // Pre-processing
    resp := next()
    // Post-processing
    return resp
}

// Chain multiple middleware
handler := http.Chain(mw1, mw2, mw3)(finalHandler)
```

Middleware can short-circuit by returning without calling `next()`:

```go
func AuthMiddleware(ctx *http.Context, next func() *http.Response) *http.Response {
    if ctx.Header("Authorization") == "" {
        return http.Unauthorized()
    }
    return next()
}
```

### Built-in Middleware

#### Recovery

Catches panics in handlers and returns a 500 response instead of crashing:

```go
server.Use(http.Recovery())
```

#### RequestID

Extracts `X-Request-ID` or `X-Cloud-Trace-Context` from the request and propagates it in the response:

```go
server.Use(http.RequestID())
```

The resolved ID is stored on `ctx.RequestID`, injected into the request
`context.Context`, and bridged into `logger.TraceIDFromContext`, so context-aware
logs (`log.InfoCtx(ctx, ...)`) correlate to the request automatically. Read it
from a bare context with `http.RequestIDFromContext(ctx)`. A telemetry-provided
`logger.TraceIDFromContext` takes precedence over the default bridge.

#### Logging

Logs each request once with method, path, status code, and duration. Server errors
use `error`; handled non-5xx responses use `info`:

```go
server.Use(http.Logging(http.LoggerOptions{
    Logger:  myLogger,                       // Optional custom logger
    Exclude: []string{"/_/health", "/_/"},   // Skip logging for these prefixes
}))
```

#### Rate Limiting

Limits requests per client using a fixed window with automatic key extraction from `X-Forwarded-For` or the remote address:

```go
server.Use(http.RateLimit(http.RateLimitOptions{
    WindowMs: 60_000,   // 1 minute window (default)
    Max:      100,       // Max requests per window (default)
    KeyFunc:  nil,       // Custom key function (optional)
    Message:  "",        // Custom error message (optional)
}))
```

When the limit is exceeded, returns `429 Too Many Requests` with `Retry-After` and `RateLimit-*` headers:
- `RateLimit-Limit` — maximum requests allowed
- `RateLimit-Remaining` — requests remaining in the current window
- `RateLimit-Reset` — Unix timestamp when the window resets

#### Compression

Applies gzip compression to responses when the client supports it and the body exceeds a size threshold:

```go
server.Use(http.Compression(http.CompressionOptions{
    Threshold: 1024,  // Min body size in bytes to compress (default: 1024)
}))
```

Compresses text, JSON, XML, JavaScript, YAML, and SVG content types. Adds `Content-Encoding: gzip` and `Vary: Accept-Encoding` headers.

## Content Negotiation

Parse `Accept` headers and select the best response format:

```go
// Parse Accept header into sorted media types
types := http.ParseAccept("text/html, application/json;q=0.9, */*;q=0.1")
// types[0].Full == "text/html" (q=1.0)
// types[1].Full == "application/json" (q=0.9)
// types[2].Full == "*/*" (q=0.1)

// Select best match from offered types
best := http.NegotiateContentType(
    ctx.Accept(),
    []string{"application/json", "text/html", "text/plain"},
)
```

## Endpoint Builder

The fluent endpoint builder — validation, DI injection, security, and OpenAPI
metadata — lives in the [`go.putnami.dev/api`](../api) package as `api.Endpoint`.
There is **no** `http.Endpoint`. This package provides the request-side
primitives the builder binds to: `http.EndpointContext` and the typed
extractors `http.ParamsAs` / `http.QueryAs` / `http.BodyAs` / `http.InjectedAs`.

```go
import (
    "go.putnami.dev/api"
    "go.putnami.dev/http"
)

type CreateUserInput struct {
    Name  string `json:"name" validate:"required,minlen=2"`
    Email string `json:"email" validate:"required,email"`
}

httpServer := http.NewServerPlugin(http.ServerConfig{})
apiPlugin := api.New(httpServer)

apiPlugin.Register(api.Endpoint("POST", "/users").
    Body(api.Type[CreateUserInput]()).
    Inject("db", dbToken).
    Secure(&security.Options{Roles: []string{"admin"}}).
    Handle(func(ctx *http.EndpointContext) *http.Response {
        body, err := http.BodyAs[CreateUserInput](ctx)
        if err != nil {
            return http.JSONStatus(400, map[string]string{"error": err.Error()})
        }
        db := ctx.Injected["db"]
        _ = db // ...
        return http.JSONStatus(201, user)
    }))
```

The builder automatically:
- Validates the request body against the schema
- Resolves DI tokens from the current scope
- Returns 400 for validation errors

Register endpoints on an `api.Plugin` (via `Register`), then wire that plugin
into your application alongside the HTTP server. See the
[`api` package](../api) for the full reference.

## Health Plugin

The health plugin provides a liveness endpoint:

```go
a.Use(server)
a.Use(http.NewHealthPlugin())
// Registers GET /_/health on the application's server
// Returns 200 {"status":"ok"} when running, 503 {"status":"unavailable"} otherwise
```

Adding the plugin to the application is the only wiring step. When the
application configures, the plugin registers `GET /_/health` on the
application's single `ServerPlugin`, in any plugin order. An application that
holds no server, or several, fails configure with an error that names
`RegisterOn`.

Two calls replace the default mount. Make either one before the application
configures:

- `health.RegisterOn(server)` registers the route on the server you choose.
- `health.Handler()` returns the handler, which you mount on a route of your
  own. The plugin then registers no route.

### Dependency probes

Probes for downstream dependencies are reported alongside the liveness state.
There are two ways to contribute one:

**Auto-discovery (preferred for plugin-owned probes).** Any plugin in the
application that implements `app.HealthChecker` is registered automatically
during the health plugin's Configure phase, using the plugin's `Name()`:

```go
import "go.putnami.dev/app"

func (p *MyPlugin) CheckHealth(ctx context.Context) error { /* ... */ }

// MyPlugin satisfies app.HealthChecker; nothing else to wire.
// database.Plugin ships an implementation that pings the connection pool.
```

**Explicit registration** for probes that aren't owned by a plugin (an external
URL, an ad-hoc check):

```go
health.AddChecker("upstream", func(ctx context.Context) error { /* ... */ })
```

`AddChecker` entries win over auto-discovered probes of the same name, so a
workload can override a plugin-provided probe when needed.

## Support and contract

The SDD owner is `go`. `go.putnami.dev/http` is public, documented, maintained, and
classified `stable` in the workspace [support catalog](../../../putnami.support.json).
Before v1.0.0, a minor `0.x` release may still contain a breaking change; the
[release policy](../../../RELEASE.md) requires release notes and migration
documentation rather than strict compatibility between every pre-1.0 minor.

The [HTTP services specification](specs/http-services.json) defines the contract,
backed by four durable decisions: [the request scope follows the
response](doc/adr/0001-request-scope-follows-the-response.md), [every long-lived
resource carries a framework-owned bound](doc/adr/0002-framework-owned-bounds.md),
[a route plugin mounts itself on the application's single
server](doc/adr/0003-a-route-plugin-mounts-itself-on-the-application-server.md),
and [an idle connection outlives the proxy's idle
timeout](doc/adr/0004-an-idle-connection-outlives-the-proxy-idle-timeout.md).

`ServerConfig` resolves its own defaults, so a directly constructed config behaves
exactly like one loaded through `go.putnami.dev/config`. In particular an unset
`ShutdownTimeout` drains for ten seconds rather than expiring immediately; set a
negative value to drain under the caller's context alone.

An unset `IdleTimeout` keeps an idle keep-alive connection, HTTP/1.1 or HTTP/2,
open for 620 seconds, never for `ReadTimeout`. A proxy in front of the server
pools connections to it and must close an idle one before the server does, so
keep `IdleTimeout` above the idle timeout of every proxy in front of the server.
Google Cloud application load balancers hold idle backend connections for 600
seconds.

Regression evidence covers [routing, middleware, responses, negotiation, and the
health plugin](http_test.go), [the health plugin mounting itself on the
application's server](health_mount_test.go), [route registration and the OPTIONS
fallback](server_test.go), [graceful shutdown and default
resolution](server_shutdown_test.go), [idle keep-alive
connections](server_idle_test.go), [the request-scope transaction
boundary](scope_boundary_test.go), [dependency injection into
handlers](inject_test.go), [streaming and WebSocket bounds](stream_test.go),
[WebSocket subprotocol negotiation and RFC 6455 framing](websocket_test.go), [the
readiness marker](server_ready_test.go), [the described route
inventory](http_routes_test.go), and [concurrent request
handling](concurrent_test.go).
