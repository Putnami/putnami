# HTTP Server

The `go.putnami.dev/http` module provides an HTTP server, trie-based router, middleware chain, request/response helpers, content negotiation, and an endpoint builder with input validation and dependency injection. It integrates with the Putnami plugin lifecycle (`go.putnami.dev/app`) so the server starts and stops automatically with your application.

## Creating a Server

Create a `ServerPlugin` with a `ServerConfig` and register it on your application module:

```go
package main

import (
    "go.putnami.dev/app"
    "go.putnami.dev/http"
)

func main() {
    server := http.NewServerPlugin(http.ServerConfig{
        Port:            8080,
        ReadTimeout:     30 * time.Second,
        WriteTimeout:    30 * time.Second,
        ShutdownTimeout: 10 * time.Second,
        MaxBodySize:     1 << 20, // 1 MiB
    })

    server.GET("/hello", func(ctx *http.Context) *http.Response {
        return http.JSON(map[string]string{"message": "hello"})
    })

    application := app.New("my-app")
    application.Use(server)
    application.ListenAndServe()
}
```

All `ServerConfig` fields have sensible defaults. The `Port` field defaults to `8080` and can be overridden by the `PORT` environment variable at runtime.

## Routing

The router uses a trie-based algorithm with three pattern types:

| Pattern | Example | Description |
|---------|---------|-------------|
| Static | `/users` | Exact path match |
| Parameter | `/users/{id}` | Captures a named path segment |
| Catch-all | `/files/{path...}` | Captures the rest of the path as `path` |

### Registering Routes

The `ServerPlugin` provides convenience methods for common HTTP methods. All methods return `*ServerPlugin` for chaining:

```go
server.GET("/users", listUsers)
server.POST("/users", createUser)
server.PUT("/users/{id}", updateUser)
server.PATCH("/users/{id}", patchUser)
server.DELETE("/users/{id}", deleteUser)
```

For other methods, use `Route` directly:

```go
server.Route("OPTIONS", "/users", handleOptions)
```

### Route Options

Routes accept options that modify handler behavior:

```go
// Return 201 Created instead of 200 OK
server.Route("POST", "/users", createUser, http.WithStatusCode(201))

// Restrict accepted Content-Type
server.Route("POST", "/upload", handleUpload, http.WithAccept("application/json", "multipart/form-data"))
```

`WithAccept` returns `415 Unsupported Media Type` if the request Content-Type does not match any of the given types.

### Path Parameters

Extract path parameters from the matched route using `ctx.Param()`:

```go
server.GET("/users/{id}", func(ctx *http.Context) *http.Response {
    id := ctx.Param("id") // "42" for /users/42
    return http.JSON(map[string]string{"id": id})
})

server.GET("/users/{userId}/posts/{postId}", func(ctx *http.Context) *http.Response {
    userID := ctx.Param("userId")
    postID := ctx.Param("postId")
    // ...
})
```

### Catch-all Routes

Named catch-all routes capture one or more path segments. The captured value is available by name:

```go
server.GET("/files/{path...}", func(ctx *http.Context) *http.Response {
    filePath := ctx.Param("path") // "docs/readme.txt" for /files/docs/readme.txt
    return http.Text("Serving: " + filePath)
})
```

### HEAD Requests

HEAD requests automatically fall back to the matching GET handler if no explicit HEAD route is registered.

## Request Context

Every handler receives a `*Context` that provides access to the request, path parameters, query parameters, headers, and body.

### Query Parameters

```go
server.GET("/search", func(ctx *http.Context) *http.Response {
    query := ctx.Query("q")       // single value
    page := ctx.Query("page")     // single value
    all := ctx.QueryParams()      // url.Values (all parameters)
    return http.JSON(map[string]string{"q": query, "page": page})
})
```

### Headers

```go
func handler(ctx *http.Context) *http.Response {
    auth := ctx.Header("Authorization")
    ct := ctx.ContentType() // Content-Type without parameters
    accept := ctx.Accept()  // Accept header
    host := ctx.Host()      // Request host
    return http.JSON(nil)
}
```

### Reading the Request Body

Parse JSON directly into a struct:

```go
server.POST("/users", func(ctx *http.Context) *http.Response {
    var input struct {
        Name  string `json:"name"`
        Email string `json:"email"`
    }
    if err := ctx.Body(&input); err != nil {
        return http.JSONStatus(400, map[string]string{"error": err.Error()})
    }
    return http.JSONStatus(201, input)
})
```

For raw bytes:

```go
raw, err := ctx.RawBody()
```

### Authentication State

Middleware that authenticates users sets `ctx.User` (a `*Claims` struct). Check it with `ctx.IsSecured()`:

```go
func handler(ctx *http.Context) *http.Response {
    if !ctx.IsSecured() {
        return http.Unauthorized()
    }
    sub := ctx.User.Subject
    return http.JSON(map[string]string{"user": sub})
}
```

### Setting Response Headers

```go
ctx.SetHeader("X-Custom-Header", "value")
```

### Underlying context.Context

The `ctx.Context()` method returns the Go `context.Context`, which carries the DI scope (if a container is configured) and cancellation signals:

```go
svc, err := inject.Resolve[MyService](ctx.Context(), myToken)
```

## Response Helpers

The package provides factory functions for common response types:

| Function | Status | Content-Type |
|----------|--------|--------------|
| `JSON(data)` | 200 | `application/json` |
| `JSONStatus(status, data)` | custom | `application/json` |
| `Text(text)` | 200 | `text/plain; charset=utf-8` |
| `NoContent()` | 204 | (none) |
| `Redirect(url, status)` | custom | `Location` header |
| `NotFound()` | 404 | `application/json` |
| `Unauthorized()` | 401 | `application/json` |
| `Forbidden()` | 403 | `application/json` |
| `InternalError(message)` | 500 | `application/json` |

### Modifying Responses

Responses are immutable-style. Use `WithStatus` and `WithHeader` to derive a new response:

```go
resp := http.JSON(data).
    WithStatus(201).
    WithHeader("X-Request-ID", requestID)
```

### Custom Responses

Build a response from scratch with `NewResponse`:

```go
resp := http.NewResponse(200)
resp.Headers.Set("Content-Type", "text/csv")
resp.raw = []byte("name,email\nAlice,alice@example.com")
```

## Middleware

A `Middleware` is a function that wraps a handler. It receives the request context and a `next` function. Call `next()` to pass control down the chain, or return a `*Response` directly to short-circuit.

```go
type Middleware func(ctx *Context, next func() *Response) *Response
```

### Writing Middleware

```go
func TimingMiddleware() http.Middleware {
    return func(ctx *http.Context, next func() *http.Response) *http.Response {
        start := time.Now()
        resp := next()
        elapsed := time.Since(start)
        ctx.SetHeader("X-Response-Time", elapsed.String())
        return resp
    }
}
```

### Registering Middleware

Add middleware to the server with `Use`. Middleware executes in registration order:

```go
server.Use(http.Recovery())
server.Use(http.RequestID())
server.Use(http.Logging(http.LoggerOptions{}))
server.Use(TimingMiddleware())
```

### Short-Circuiting

Return a response without calling `next()` to stop the chain:

```go
func AuthMiddleware() http.Middleware {
    return func(ctx *http.Context, next func() *http.Response) *http.Response {
        if ctx.Header("Authorization") == "" {
            return http.Unauthorized()
        }
        return next()
    }
}
```

### Composing Middleware Manually

Use `Chain` to compose middleware into a handler wrapper outside the server:

```go
chain := http.Chain(mw1, mw2, mw3)
wrapped := chain(myHandler)
resp := wrapped(ctx)
```

### Built-in Middleware

#### Recovery

Catches panics and returns a 500 response with the panic message. Always register this first:

```go
server.Use(http.Recovery())
```

#### RequestID

Establishes a correlation ID for each request: it reuses an inbound `X-Request-ID` or `X-Cloud-Trace-Context` header when present, and generates a random one otherwise. The ID is stored on the context (`ctx.RequestID`), echoed on the response as `X-Request-ID`, and — when `Logging()` is also installed — included in the access log as `requestId`:

```go
server.Use(http.RequestID())
server.Use(http.Logging(http.LoggerOptions{})) // logs include requestId
```

Handlers can read `ctx.RequestID` to tag their own logs with the same value. The
ID is also injected into the request `context.Context`, and `RequestID()` wires a
default `logger.TraceIDFromContext` bridge, so context-aware logs emitted from a
handler or service (`log.InfoCtx(ctx, ...)`, `log.ErrorCtx(ctx, ...)`) carry the
same correlation ID with no extra setup — keeping application and error logs tied
to the access log. Use `http.RequestIDFromContext(ctx)` to read it from a bare
`context.Context`. A telemetry package that installs its own
`logger.TraceIDFromContext` takes precedence (the default bridge is only set when
none is registered).

#### Logging

Logs each request with method, path, status code, and duration. Supports excluding paths (useful for health checks):

```go
server.Use(http.Logging(http.LoggerOptions{
    Exclude: []string{"/_/health"},
}))
```

Requests resulting in 5xx status codes are logged at error level; all others at info level.

#### Rate Limiting

Limits requests per client using a sliding time window. Adds `RateLimit-Limit`, `RateLimit-Remaining`, and `RateLimit-Reset` response headers:

```go
server.Use(http.RateLimit(http.RateLimitOptions{
    WindowMs: 60_000, // 1 minute
    Max:      100,    // 100 requests per window
}))
```

Options:

| Field | Default | Description |
|-------|---------|-------------|
| `WindowMs` | `60000` | Time window in milliseconds |
| `Max` | `100` | Maximum requests per window |
| `KeyFunc` | IP-based | Function to extract the rate limit key from the context |
| `Message` | `"Too Many Requests"` | Response body message |
| `Headers` | `true` | Include `RateLimit-*` response headers |

The default key function uses `X-Forwarded-For` (first IP) or falls back to the remote address.

#### Compression

Applies gzip compression when the client sends `Accept-Encoding: gzip` and the response body exceeds the threshold. Only compresses text-based content types (`text/*`, `application/json`, `application/xml`, `application/javascript`, `application/yaml`, `image/svg+xml`):

```go
server.Use(http.Compression(http.CompressionOptions{
    Threshold: 1024, // minimum bytes to compress (default: 1024)
}))
```

## Endpoint Builder

The fluent endpoint builder provides automatic input validation, dependency injection, OpenAPI metadata, and per-endpoint middleware. It lives in the [`go.putnami.dev/api`](../../api) package as **`api.Endpoint`** — there is no `http.Endpoint`. This package supplies the request-side primitives the builder binds to: `http.EndpointContext` and the typed extractors `http.ParamsAs`, `http.QueryAs`, `http.BodyAs`, and `http.InjectedAs`.

Endpoints are registered on an `api.Plugin`, which dispatches them onto an `*http.ServerPlugin`:

```go
import (
    "go.putnami.dev/api"
    "go.putnami.dev/http"
)

httpServer := http.NewServerPlugin(http.ServerConfig{})
apiPlugin := api.New(httpServer)
// ... register endpoints on apiPlugin (below) ...
app.New("myapp").Use(httpServer).Use(apiPlugin).ListenAndServe()
```

### Basic Usage

```go
type CreateUserBody struct {
    Name  string `json:"name" validate:"required,minlen=2"`
    Email string `json:"email" validate:"required,email"`
}

apiPlugin.Register(api.Endpoint("POST", "/users").
    Description("Create a new user").
    Body(api.Type[CreateUserBody]()).
    Returns(api.Type[User]()).
    Throws(409, "User already exists", nil).
    Handle(func(ctx *http.EndpointContext) *http.Response {
        body, err := http.BodyAs[CreateUserBody](ctx)
        if err != nil {
            return http.JSONStatus(400, map[string]string{"error": err.Error()})
        }
        // create user...
        return http.JSONStatus(201, user)
    }))
```

When validation fails, the endpoint automatically returns a 400 response with structured error details. Note that `Returns` and `Throws` take schema types (`api.Type[T]()`, or `nil` for no body), not free-form description strings.

### Validating Path and Query Parameters

```go
type GetUserParams struct {
    ID string `json:"id" validate:"required,uuid"`
}

type ListUsersQuery struct {
    Page  int `json:"page" validate:"min=1" default:"1"`
    Limit int `json:"limit" validate:"min=1,max=100" default:"20"`
}

apiPlugin.Register(api.Endpoint("GET", "/users/{id}").
    Params(api.Type[GetUserParams]()).
    Handle(func(ctx *http.EndpointContext) *http.Response {
        params, _ := http.ParamsAs[GetUserParams](ctx)
        return http.JSON(map[string]string{"id": params.ID})
    }))

apiPlugin.Register(api.Endpoint("GET", "/users").
    Query(api.Type[ListUsersQuery]()).
    Handle(func(ctx *http.EndpointContext) *http.Response {
        query, _ := http.QueryAs[ListUsersQuery](ctx)
        return http.JSON(map[string]any{"page": query.Page})
    }))
```

### Dependency Injection

Inject services resolved from the DI container into the endpoint handler:

```go
apiPlugin.Register(api.Endpoint("GET", "/users").
    Inject("repo", userRepoToken).
    Handle(func(ctx *http.EndpointContext) *http.Response {
        repo, err := http.InjectedAs[*UserRepository](ctx, "repo")
        if err != nil {
            return http.InternalError(err.Error())
        }
        users, err := repo.List(ctx.Context())
        if err != nil {
            return http.InternalError(err.Error())
        }
        return http.JSON(users)
    }))
```

### Per-Endpoint Middleware

Add middleware that only applies to a specific endpoint:

```go
apiPlugin.Register(api.Endpoint("DELETE", "/users/{id}").
    Use(adminOnlyMiddleware).
    HandleRaw(func(ctx *http.Context) *http.Response {
        // ...
    }))
```

### Raw Handlers

Use `HandleRaw` instead of `Handle` when you do not need validation or the `EndpointContext`:

```go
apiPlugin.Register(api.Endpoint("GET", "/ping").
    HandleRaw(func(ctx *http.Context) *http.Response {
        return http.Text("pong")
    }))
```

See the [`go.putnami.dev/api`](../../api) package for the full builder reference.

## Content Negotiation

Parse `Accept` headers and select the best content type for the response:

```go
// Parse Accept header into sorted media types
types := http.ParseAccept("text/html, application/json;q=0.9, */*;q=0.1")
// types[0].Full == "text/html" (quality 1.0)
// types[1].Full == "application/json" (quality 0.9)

// Select best match from offered types
best := http.NegotiateContentType(ctx.Accept(), []string{"application/json", "text/html"})
```

## Health Checks

The `HealthPlugin` provides a liveness/readiness endpoint at `/_/health`. It returns 200 when the application is running and 503 during startup or shutdown:

```go
application := app.New("my-app")
application.Use(server)
application.Use(http.NewHealthPlugin())
```

Adding the plugin is the only wiring step. When the application configures, the plugin registers `GET /_/health` on the application's single server, in any plugin order. An application that holds no server, or several, fails configure with an error that names `RegisterOn`.

To choose the server, call `RegisterOn` before the application configures. To serve the probe on another path, take the handler: the plugin then registers no route.

```go
health := http.NewHealthPlugin()
health.RegisterOn(adminServer)              // choose the server
// or
server.GET("/status", health.Handler())     // choose the route
```

Exclude the health endpoint from logging to reduce noise:

```go
server.Use(http.Logging(http.LoggerOptions{
    Exclude: []string{"/_/health"},
}))
```

## Dependency Injection Scoping

When the server is used as an `app.Plugin`, the DI container is acquired automatically via `owner.Container()` in `Configure`. Each incoming request gets its own DI scope. This scope is created before the handler runs and closed after the response is written. Access scoped services through `ctx.Context()`:

```go
// Container is acquired automatically when used as a plugin.
// a.Use(server) // server gets the container from the app module

server.GET("/users", func(ctx *http.Context) *http.Response {
    repo, err := inject.Resolve[*UserRepository](ctx.Context(), userRepoToken)
    if err != nil {
        return http.InternalError(err.Error())
    }
    // repo is scoped to this request
    return http.JSON(repo.List())
})
```

## Error Codes

The package defines typed error codes for structured error handling:

| Code | Description |
|------|-------------|
| `http.listen` | Server failed to bind to the address |
| `http.body` | Error reading or closing the request body |
| `http.scope` | Error creating or closing a DI scope |
| `http.no_server` | A plugin that mounts itself finds no `ServerPlugin` in the application |
| `http.ambiguous_server` | A plugin that mounts itself finds several `ServerPlugin`s in the application |

## Best Practices

- Register `Recovery()` as the first middleware to catch panics in all subsequent middleware and handlers.
- Use `--impacted` with the putnami CLI during development: `putnami test --impacted`.
- Prefer the `EndpointBuilder` for routes that need input validation. It provides consistent 400 error responses with structured validation details.
- Use `ctx.Param()` for path parameters and `ctx.Query()` for query strings. Do not parse `ctx.Path` manually.
- Return `nil` from a handler only if you have already written to `ctx.Writer` directly. In all other cases, return a `*Response`.
- Keep middleware focused on a single concern (logging, auth, rate limiting). Compose them with `server.Use()`.
- Use the `HealthPlugin` in production deployments. Load balancers and orchestrators (Kubernetes, Cloud Run) rely on health endpoints for readiness checks.
- Set `PORT` via environment variable in container deployments instead of hardcoding it in `ServerConfig`.

## Contract and compatibility

See the [HTTP services specification](../specs/http-services.json), the
[request-scope ADR](adr/0001-request-scope-follows-the-response.md), the
[bounded-resource ADR](adr/0002-framework-owned-bounds.md), the
[route-plugin ADR](adr/0003-a-route-plugin-mounts-itself-on-the-application-server.md),
and [support
evidence](../README.md#support-and-contract). The package is stable and
maintained, with the workspace's [pre-1.0 migration
policy](../../../../RELEASE.md), not strict compatibility between every `0.x`
minor.
