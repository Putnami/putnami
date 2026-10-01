# Go Framework

The Putnami Go framework follows the same Putnami architecture in idiomatic Go. It provides the same core abstractions — dependency injection, HTTP server, security, events, storage, and service clients — using Go's strengths: goroutines, channels, generics, `context.Context`, and interfaces.

## Installation

All Go packages use the `go.putnami.dev` import domain. Its `go-import` meta tags name the module proxy at `https://go.putnami.dev`. The public module proxy, `proxy.golang.org`, holds some published versions.

The quickest start is a Putnami project: `putnami projects create api --template go-server` pins the newest framework version your Go module proxies serve. To add a package to another module, name the version:

```bash
go get go.putnami.dev/app@<version>
go get go.putnami.dev/http@<version>
go get go.putnami.dev/inject@<version>
```

Today `go.putnami.dev` does not serve anonymous reads, so a reader without access to it gets only what `proxy.golang.org` holds:

- `go get` without a version fails for these modules, because the version list of `proxy.golang.org` names a version it no longer serves.
- At the version `https://proxy.golang.org/go.putnami.dev/app/@latest` names, `inject`, `config` and `logger` resolve, but `app` and `http` do not. They import `go.putnami.dev/protocol/diagnostic`, which `proxy.golang.org` does not hold at that version.
- A `go-server` project requires `app` and `http`, so it does not build without access to `go.putnami.dev`.

## Package overview

| Package | Description |
|---------|-------------|
| [`inject`](/docs/frameworks/go/dependency-injection) | Hierarchical DI container with named tokens, generics, scoped providers, cycle detection |
| [`config`](/docs/frameworks/go/configuration) | Multi-source configuration (YAML, env vars, maps) with struct tags and DI bridge |
| [`logger`](/docs/frameworks/go/logging) | Structured logging via `slog` with pluggable sinks (JSON default, console, buffer, memory) |
| [`schema`](/docs/frameworks/go/validation) | Struct-tag-based validation with type coercion |
| [`errors`](/docs/frameworks/go/errors) | Structured error model with typed codes, categories, stack capture, HTTP mapping |
| [`app`](/docs/frameworks/go/plugins-and-lifecycle) | Plugin-based application lifecycle with module composition and optional fx-style DI |
| [`http`](/docs/frameworks/go/http) | HTTP server, trie-based router, middleware (recovery, logging, rate limiting, compression) |
| [`security`](/docs/frameworks/go/security) | Declarative authorization middleware (roles, scopes, custom guards) |
| [`grpc`](/docs/frameworks/go/grpc) | gRPC server with Connect protocol gateway and DI-scoped requests |
| [`openapi`](/docs/frameworks/go/openapi) | OpenAPI 3.0.3 spec generation from Go struct types |
| [`sql`](/docs/frameworks/go/persistence) | PostgreSQL via pgx — connection pool, repository pattern, migrations, query builder |
| [`cache`](/docs/frameworks/go/caching) | Layered caching (memory + disk) with TTL and FIFO eviction |
| [`telemetry`](/docs/frameworks/go/telemetry) | OpenTelemetry integration for distributed tracing and metrics |
| [`events`](/docs/frameworks/go/events) | Typed event system with topics, handlers, retry, and dead-letter queues |
| [`storage`](/docs/frameworks/go/storage) | Object storage with pluggable backends (memory, filesystem, S3) |
| [`client`](/docs/frameworks/go/service-clients) | Runtime for generated first-party clients: bindings, credentials, typed errors, streams |

## Quick start

### Explicit wiring (idiomatic Go)

```go
package main

import (
    "context"

    "go.putnami.dev/app"
    "go.putnami.dev/config"
    fhttp "go.putnami.dev/http"
)

type AppConfig struct {
    Port int    `json:"port" default:"8080" env:"PORT"`
    DSN  string `json:"dsn" env:"DATABASE_URL"`
}

func main() {
    cfg, _ := config.Load(config.Config[AppConfig]("app"))
    db := connectDB(cfg.DSN)
    users := NewUserService(db)

    server := fhttp.NewServerPlugin(fhttp.ServerConfig{Port: cfg.Port})
    server.Use(fhttp.Recovery())
    server.GET("/users", listUsersHandler(users))

    a := app.New("my-service")
    a.Module.Use(server)
    a.Run(func(ctx context.Context) error {
        <-ctx.Done()
        return nil
    })
    a.ListenAndServe()
}
```

### Constructor-based DI (fx-style)

```go
func main() {
    a := app.New("my-service")
    a.ProvideFunc(
        LoadConfig,
        connectDB,
        NewUserService,
        newHTTPServer,
    )
    a.InvokeFunc(func(server *fhttp.ServerPlugin, users *UserService) {
        server.GET("/users", listUsersHandler(users))
    })
    a.ListenAndServe()
}
```

Both approaches can be mixed freely. DI is entirely optional — applications that register no providers skip container creation entirely.

## Architecture

The framework is organized into four layers:

```
Foundation:      errors, logger, config, schema
DI:              inject (optional, standalone)
Application:     app → http, grpc, sql, events, storage, cache, client
Cross-cutting:   security, telemetry, openapi
```

Every component is a **plugin** that participates in the application lifecycle. Plugins compose into **modules**, and modules form a tree rooted at the application. See [Plugins & Lifecycle](/docs/frameworks/go/plugins-and-lifecycle) for details.

## Design principles

- **Stdlib only** — no external runtime dependencies (except pgx for PostgreSQL and gRPC)
- **Optional DI** — wire services explicitly or use constructor-based DI; both compose naturally
- **Generics** — type-safe topics, repositories, caches, and tokens via Go 1.18+ generics
- **Context-driven** — transactions, scopes, loggers, and traces all flow through `context.Context`
- **Plugin architecture** — every framework component implements the same lifecycle interface
- **Goroutine-safe** — shared state uses `sync.Mutex`, channels, or atomics

## TypeScript ↔ Go comparison

| Concept | TypeScript | Go |
|---------|-----------|-----|
| DI tokens | `named<T>('key')` | `inject.Named[T]("key")` |
| DI scope | `AsyncLocalStorage` | `context.Context` |
| Event topics | `topic('name', schema)` | `events.NewTopic[T]("name")` |
| Event handlers | `handler(topic).handle(fn)` | `events.Handle(topic, fn, opts...)` |
| Storage buckets | `Bucket('name', opts)` | `storage.Bucket("name", opts...)` |
| Client builder | `ClientBuilder.for(Client)` | `client.NewBuilder()` |
| Circuit breaker | Custom implementation | `client.CircuitBreaker` |
| Config | `Config('name', schema)` | `config.Config[T]("name")` |
| Validation | Schema DSL | Struct tags |
| Application | `application().use(plugin)` | `app.New("name").Module.Use(plugin)` |

## Related documentation

- [HTTP & Middleware](/docs/frameworks/go/http)
- [Dependency Injection](/docs/frameworks/go/dependency-injection)
- [Plugins & Lifecycle](/docs/frameworks/go/plugins-and-lifecycle)
- [TypeScript framework](/docs/frameworks/typescript)
