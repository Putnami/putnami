# HTTP & Middleware

`go.putnami.dev/http` provides the HTTP server, trie-based routing, and middleware layer built on Go's `net/http` standard library.

## HTTP server

### Basic setup

```go
package main

import (
    "context"

    "go.putnami.dev/app"
    fhttp "go.putnami.dev/http"
)

func main() {
    server := fhttp.NewServerPlugin(fhttp.ServerConfig{Port: 3000})
    server.Use(fhttp.Recovery())
    server.Use(fhttp.Logging(fhttp.LoggerOptions{}))
    server.GET("/health", func(ctx *fhttp.Context) *fhttp.Response {
        return fhttp.JSON(map[string]string{"status": "ok"})
    })

    a := app.New("my-service")
    a.Module.Use(server)
    a.ListenAndServe()
}
```

### Configuration

```go
fhttp.NewServerPlugin(fhttp.ServerConfig{
    Port:            8080,           // Server port (env: PORT)
    ReadTimeout:     30 * time.Second,
    WriteTimeout:    30 * time.Second,
    IdleTimeout:     620 * time.Second,
    ShutdownTimeout: 10 * time.Second,
    MaxBodySize:     1 << 20,        // 1 MiB
})
```

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `Port` | `int` | `8080` | Server port (`PORT` env var) |
| `ReadTimeout` | `time.Duration` | `30s` | Read timeout |
| `WriteTimeout` | `time.Duration` | `30s` | Write timeout |
| `IdleTimeout` | `time.Duration` | `620s` | Idle keep-alive timeout, HTTP/1.1 and HTTP/2 |
| `ShutdownTimeout` | `time.Duration` | `10s` | Graceful shutdown timeout |
| `MaxBodySize` | `int64` | `1048576` | Max request body size in bytes |

`IdleTimeout` must exceed the idle timeout of every proxy in front of the
server. A proxy pools connections to the server, and when the server closes an
idle one first, the proxy can send a request on a connection that is closing:
the client gets an error from the proxy and the handler never runs. Google
Cloud application load balancers hold idle backend connections for 600 seconds
and [ask for a longer backend
timeout](https://docs.cloud.google.com/load-balancing/docs/https/request-distribution#timeout-keepalive-backends),
hence the 620-second default. Unlike `net/http`, an unset `IdleTimeout` never
falls back to `ReadTimeout`.

## Routing

### Basic routes

```go
server := fhttp.NewServerPlugin(fhttp.ServerConfig{Port: 3000})

// GET
server.GET("/users", func(ctx *fhttp.Context) *fhttp.Response {
    return fhttp.JSON([]User{})
})

// POST
server.POST("/users", func(ctx *fhttp.Context) *fhttp.Response {
    var body CreateUserRequest
    if err := ctx.Body(&body); err != nil {
        return fhttp.JSONStatus(400, map[string]string{"error": "invalid body"})
    }
    return fhttp.JSONStatus(201, user)
})

// PUT
server.PUT("/users/{id}", func(ctx *fhttp.Context) *fhttp.Response {
    id := ctx.Param("id")
    return fhttp.JSON(map[string]string{"updated": id})
})

// DELETE
server.DELETE("/users/{id}", func(ctx *fhttp.Context) *fhttp.Response {
    id := ctx.Param("id")
    return fhttp.NoContent()
})

// PATCH
server.PATCH("/users/{id}", func(ctx *fhttp.Context) *fhttp.Response {
    id := ctx.Param("id")
    return fhttp.JSON(map[string]string{"patched": id})
})
```

### Route with options

```go
server.Route("GET", "/api/data", handler,
    fhttp.WithAccept("application/json"),
    fhttp.WithStatusCode(200),
)
```

### Route patterns

The router uses a trie-based matcher supporting three pattern types:

| Pattern | Example | Description |
|---------|---------|-------------|
| Exact | `/users` | Matches the exact path |
| Parameter | `/users/{id}` | Captures a named segment |
| Catch-all | `/files/{path...}` | Captures the remaining path segments in a named parameter |

```go
server.GET("/users/{id}", func(ctx *fhttp.Context) *fhttp.Response {
    id := ctx.Param("id")      // "123" for /users/123
    return fhttp.JSON(map[string]string{"id": id})
})

server.GET("/files/{path...}", func(ctx *fhttp.Context) *fhttp.Response {
    path := ctx.Param("path")  // "docs/readme.md" for /files/docs/readme.md
    return fhttp.Text(path)
})
```

## Request context

### Properties

```go
func handler(ctx *fhttp.Context) *fhttp.Response {
    ctx.Request     // *http.Request
    ctx.Writer      // http.ResponseWriter
    ctx.Method      // HTTP method string
    ctx.Path        // URL path
    ctx.Route       // Matched route pattern (e.g., "/users/{id}")
    ctx.Params      // map[string]string of path parameters
    ctx.User        // map[string]any of authenticated user claims
    ctx.StatusCode  // Response status code (modifiable)
}
```

### Methods

```go
func handler(ctx *fhttp.Context) *fhttp.Response {
    // Path and query parameters
    id := ctx.Param("id")
    page := ctx.Query("page")
    params := ctx.QueryParams()    // url.Values

    // Headers
    auth := ctx.Header("Authorization")
    ctx.SetHeader("X-Custom", "value")

    // Host and security
    host := ctx.Host()
    secure := ctx.IsSecured()      // true if user claims are set

    // Body parsing
    var body MyStruct
    if err := ctx.Body(&body); err != nil {
        return fhttp.JSONStatus(400, map[string]string{"error": err.Error()})
    }

    // Raw body
    raw, err := ctx.RawBody()

    // Content negotiation
    ct := ctx.ContentType()
    accept := ctx.Accept()

    // Access underlying context.Context
    goCtx := ctx.Context()
}
```

## Response helpers

### Factory functions

```go
// JSON responses
fhttp.JSON(data)                     // 200 + JSON
fhttp.JSONStatus(201, data)          // Custom status + JSON

// Text
fhttp.Text("Hello, world!")          // 200 + text/plain

// Redirect
fhttp.Redirect("/new-location", 302)

// No content
fhttp.NoContent()                    // 204

// Error responses
fhttp.NotFound()                     // 404 JSON
fhttp.Unauthorized()                 // 401 JSON
fhttp.Forbidden()                    // 403 JSON
fhttp.InternalError("something failed") // 500 JSON
```

### Response modification

```go
fhttp.JSON(data).
    WithHeader("Cache-Control", "no-store").
    WithHeader("X-Request-Id", requestId).
    WithStatus(201)
```

## Middleware

### Middleware signature

```go
type Middleware func(ctx *Context, next func() *Response) *Response
```

### Creating middleware

```go
// Logging middleware
func requestLogger() fhttp.Middleware {
    return func(ctx *fhttp.Context, next func() *fhttp.Response) *fhttp.Response {
        start := time.Now()
        resp := next()
        duration := time.Since(start)
        fmt.Printf("%s %s %dms\n", ctx.Method, ctx.Path, duration.Milliseconds())
        return resp
    }
}

// Authentication middleware
func requireAuth() fhttp.Middleware {
    return func(ctx *fhttp.Context, next func() *fhttp.Response) *fhttp.Response {
        token := ctx.Header("Authorization")
        if token == "" {
            return fhttp.Unauthorized()
        }
        claims, err := verifyToken(token)
        if err != nil {
            return fhttp.Unauthorized()
        }
        ctx.User = claims
        return next()
    }
}
```

### Registering middleware

```go
server := fhttp.NewServerPlugin(fhttp.ServerConfig{Port: 3000})

// Middleware executes in registration order
server.Use(fhttp.Recovery())        // 1st — catch panics
server.Use(fhttp.RequestID())       // 2nd — extract trace ID
server.Use(fhttp.Logging(fhttp.LoggerOptions{})) // 3rd — log requests
server.Use(requireAuth())           // 4th — check authentication
```

### Early return

Middleware can return early without calling `next()`:

```go
func maintenanceMode() fhttp.Middleware {
    return func(ctx *fhttp.Context, next func() *fhttp.Response) *fhttp.Response {
        if isMaintenanceMode() {
            return fhttp.JSONStatus(503, map[string]string{
                "error": "Service temporarily unavailable",
            })
        }
        return next()
    }
}
```

### Modifying responses

```go
func securityHeaders() fhttp.Middleware {
    return func(ctx *fhttp.Context, next func() *fhttp.Response) *fhttp.Response {
        resp := next()
        if resp != nil {
            return resp.
                WithHeader("X-Content-Type-Options", "nosniff").
                WithHeader("X-Frame-Options", "DENY")
        }
        return resp
    }
}
```

### Chaining middleware

```go
// Compose middleware into a single handler wrapper
protected := fhttp.Chain(
    fhttp.Recovery(),
    fhttp.RequestID(),
    requireAuth(),
)

handler := protected(func(ctx *fhttp.Context) *fhttp.Response {
    return fhttp.JSON(map[string]string{"ok": "true"})
})
```

## Built-in middleware

### Recovery

Catches panics and returns a 500 response:

```go
server.Use(fhttp.Recovery())
```

### Request ID

Extracts trace ID from incoming headers (`X-Request-ID`, `X-Cloud-Trace-Context`) and makes it available in the request context:

```go
server.Use(fhttp.RequestID())
```

### Logging

Logs each request with method, path, status, and duration:

```go
server.Use(fhttp.Logging(fhttp.LoggerOptions{
    Logger:  customLogger,           // nil = default logger
    Exclude: []string{"/health"},    // path prefixes to skip
}))
```

### Rate limiting

Sliding-window rate limiting per client:

```go
server.Use(fhttp.RateLimit(fhttp.RateLimitOptions{
    WindowMs: 60_000,    // Time window (default: 60000ms)
    Max:      100,       // Max requests per window (default: 100)
    Message:  "Too Many Requests",
    Headers:  boolPtr(true),  // Include RateLimit-* headers
    KeyFunc:  func(ctx *fhttp.Context) string {
        return ctx.Header("X-Forwarded-For") // Custom key extraction
    },
}))
```

### Compression

Gzip compression for responses above a size threshold:

```go
server.Use(fhttp.Compression(fhttp.CompressionOptions{
    Threshold: 1024, // Minimum body size in bytes (default: 1024)
}))
```

## Endpoint builder

For endpoints that need validation, DI injection, and OpenAPI metadata, use the fluent endpoint builder:

```go
import (
    fhttp "go.putnami.dev/http"
    "go.putnami.dev/inject"
)

type CreateUserParams struct {
    Name  string `json:"name" validate:"required,minlen=2"`
    Email string `json:"email" validate:"required,email"`
}

endpoint := fhttp.Endpoint("POST", "/users").
    Description("Create a new user").
    Body(reflect.TypeOf(CreateUserParams{})).
    Returns("The created user").
    Throws(409, "Email already exists").
    Inject("users", inject.TokenOf[*UserService]()).
    Handle(func(ctx *fhttp.EndpointContext) *fhttp.Response {
        users := ctx.Injected["users"].(*UserService)
        body := ctx.ValidatedBody
        // ... create user
        return fhttp.JSONStatus(201, user)
    })

endpoint.Register(server)
```

## Health checks

For new code, use [`go.putnami.dev/platform`](./19-platform-endpoints.md) — it mounts `/healthz`, `/livez`, `/readyz`, `/version`, and (opt-in) `/debug/pprof/*`, and auto-discovers `app.HealthChecker` / `app.ReadinessChecker` probes from the module tree.

The `http` package's legacy health plugin remains available for a single liveness endpoint at `/_/health`:

```go
a := app.New("my-service")
a.Module.Use(server)
a.Module.Use(fhttp.NewHealthPlugin())
```

Adding the plugin is the only wiring step: when the application configures, the plugin registers `GET /_/health` on the application's single server. An application that holds no server, or several, fails configure with an error that names `RegisterOn`. Call `health.RegisterOn(server)` before the application configures to choose the server, or mount `health.Handler()` on a route of your own.

Returns `200 {"status":"ok"}` when ready and `503 {"status":"unavailable"}` during startup or shutdown. It also auto-discovers `app.HealthChecker` implementations and reports their state under a `checks` map — but it has no readiness, version, or pprof surface. Prefer the platform plugin unless you specifically need to keep the `/_/health` path.

## Content negotiation

```go
import fhttp "go.putnami.dev/http"

// Parse Accept header
types := fhttp.ParseAccept("text/html, application/json;q=0.9")
// types[0].Full == "text/html", types[1].Full == "application/json"

// Negotiate content type
best := fhttp.NegotiateContentType(
    ctx.Accept(),
    []string{"application/json", "text/html"},
)
```

## DI integration

The server plugin creates a DI scope per request when used as an `app.Plugin`. The container is acquired automatically via `owner.Container()` in `Configure`. This enables per-request scoped services:

```go
// Container is acquired automatically when server is added as a plugin.
// a.Use(server)

// In a handler, resolve scoped services from the request context
server.GET("/users", func(ctx *fhttp.Context) *fhttp.Response {
    userService, _ := inject.Resolve[*UserService](ctx.Context(), inject.TokenOf[*UserService]())
    return fhttp.JSON(userService.List(ctx.Context()))
})
```

See [Dependency Injection](/docs/frameworks/go/dependency-injection) for scoped provider details.

## Request scope and the response

Each request runs in its own dependency scope. The scope is reconciled against
the response the client actually receives, not against a returned error:

| Outcome | Scope |
|---------|-------|
| 2xx or 3xx response | commit |
| 4xx response | commit — a client error is a handled outcome, and any audit row or counter written alongside it is kept |
| 5xx response | roll back |
| Canceled or timed-out request | roll back |
| Panic no middleware converted | roll back, then the panic is re-raised |
| Commit itself fails after a success | the prepared response is replaced by a sanitized 500 |

A handler that wants a 4xx to discard its work marks its unit of work
rollback-only; the status code alone will not do it.

## Graceful shutdown and bounded resources

`ServerConfig`'s `default:` struct tags are applied by `config.Load`. A directly
constructed config — the shape used throughout these guides — carries Go zero
values, so the server resolves every bound itself and both construction paths
behave identically:

| Field | Unset | Opt out |
|-------|-------|---------|
| `ShutdownTimeout` | 10s drain for in-flight requests | negative: drain under the caller's context alone |
| `WebSocketIdleTimeout` | 60s idle deadline on hijacked connections | negative disables |
| `StreamWriteTimeout` | 30s per server-sent-event write | negative disables |
| `ReadTimeout` / `WriteTimeout` | 30s | — |
| `IdleTimeout` | 620s idle keep-alive, HTTP/1.1 and HTTP/2, never `ReadTimeout` | — |
| `MaxBodySize` | 1 MiB | — |
| `MaxHeaderBytes` | 64 KiB | — |

```go
server := fhttp.NewServerPlugin(fhttp.ServerConfig{Port: 8080})
// Stop() drains in-flight requests for 10s before closing.
```

## Related guides

- [Security](/docs/frameworks/go/security) — authorization middleware
- [Plugins & Lifecycle](/docs/frameworks/go/plugins-and-lifecycle) — server lifecycle
- [Dependency Injection](/docs/frameworks/go/dependency-injection) — per-request scoping
- [OpenAPI](/docs/frameworks/go/openapi) — spec generation from endpoints
## Support and contract

`go.putnami.dev/http` is `stable` in the workspace support catalog. Its behavior
is defined by the HTTP services specification and two accepted decision records
next to the package source (`go/framework/http/specs/` and
`go/framework/http/doc/adr/`). Before v1.0.0 a minor `0.x` release may still
contain a documented breaking change; strict compatibility between every pre-1.0
minor is not promised.
