# Putnami Go Framework

A modular Go backend framework for building service infrastructure: HTTP, gRPC, data access, events, storage, caching, observability, and dependency injection — assembled through a plugin-based application lifecycle.

```go
a := app.New("my-service")

a.ProvideFunc(LoadConfig, NewDB, NewUserService)

a.InvokeFunc(func(server *http.ServerPlugin, users *UserService) {
    server.GET("/users", listUsers(users))
    server.POST("/users", createUser(users))
    server.GET("/users/{id}", getUser(users))
})

a.ListenAndServe()
```

## Principles

- **Stdlib first** — 14 of 17 packages have zero external dependencies. Only `sql`, `grpc`, and `telemetry` use external libraries (pgx, grpc-go, OpenTelemetry).
- **Plugin lifecycle** — Plugins implement Generate, Configure, Start, Stop. The application orchestrates them in order, with OS signal handling and graceful shutdown.
- **DI is optional** — Use constructor-based DI (fx-style), token-based DI (explicit), or wire everything by hand. The framework works all three ways.
- **Idiomatic Go** — `context.Context`, struct tags, interfaces, goroutine-safe internals. No TypeScript patterns ported over.

## Packages

All packages are standalone Go modules under `go.putnami.dev/`.

### Foundation

| Package | Module | Description |
|---------|--------|-------------|
| [inject](inject/) | `go.putnami.dev/inject` | Hierarchical DI container — tokens, constructors, scopes, visibility, cycle detection, fork for testing |
| [errors](errors/) | `go.putnami.dev/errors` | Structured error model — codes, categories, HTTP mapping, retryable, stack capture |
| [logger](logger/) | `go.putnami.dev/logger` | Structured logging — console, JSON (Cloud Logging), buffer, and memory sinks |
| [schema](schema/) | `go.putnami.dev/schema` | Struct-tag validation — required, uuid, email, url, min/max, pattern, oneof, coercion |
| [config](config/) | `go.putnami.dev/config` | Typed config loading — YAML, env vars, maps, source priority, DI integration |

### Application & HTTP

| Package | Module | Description |
|---------|--------|-------------|
| [app](app/) | `go.putnami.dev/app` | Plugin lifecycle, module tree, DI wiring (ProvideFunc, InvokeFunc), signal handling |
| [http](http/) | `go.putnami.dev/http` | Trie router, request context, response helpers, middleware chain, endpoint builder, health check |
| [security](security/) | `go.putnami.dev/security` | Authorization middleware — roles, scopes, client IDs, custom guards, identity resolution |
| [openapi](openapi/) | `go.putnami.dev/openapi` | OpenAPI 3.0.3 spec generation from route definitions and Go structs |

### Data & Messaging

| Package | Module | Description |
|---------|--------|-------------|
| [database](database/) | `go.putnami.dev/database` | PostgreSQL — pgx pool, repository pattern, schema migrations (Putnami migration protocol), transactions, query builder |
| [events](events/) | `go.putnami.dev/events` | Typed topics, handler builders, memory broker, competing/broadcast distribution, retry with DLQ |
| [storage](storage/) | `go.putnami.dev/storage` | Object storage — memory, filesystem, and S3 backends with bucket definitions and constraints |
| [cache](cache/) | `go.putnami.dev/cache` | Memory and disk caches with TTL, FIFO eviction, and L1/L2 layered promotion |

### Transport & Observability

| Package | Module | Description |
|---------|--------|-------------|
| [grpc](grpc/) | `go.putnami.dev/grpc` | gRPC server, Connect protocol gateway, interceptors (logging, recovery, DI) |
| [client](client/) | `go.putnami.dev/client` | HTTP client builder — retry with backoff, circuit breaker, interceptor chain |
| [telemetry](telemetry/) | `go.putnami.dev/telemetry` | OpenTelemetry — tracing, metrics, HTTP middleware, span helpers |

## Dependency Graph

```
                         ┌─────────┐
                         │  errors │
                         └────┬────┘
              ┌───────┬───────┼───────┬──────────┬──────────┐
              v       v       v       v          v          v
          ┌──────┐┌──────┐┌──────┐┌───────┐┌─────────┐┌────────┐
          │inject││logger││events││storage││  client ││  cache │
          └──┬───┘└──┬───┘└──────┘└───────┘└─────────┘└────────┘
             │       │
         ┌───┴───┐   │
         │config │   │
         └───┬───┘   │
             └───┬───┘
                 v
            ┌─────────┐
            │   app   │
            └────┬────┘
          ┌──────┼──────────────────────┐
          v      v                      v
       ┌─────┐┌────┐              ┌─────────┐
       │ sql ││http│              │  grpc   │
       └─────┘└─┬──┘              └─────────┘
          ┌─────┼──────┐
          v     v      v
     ┌────────┐┌───────┐┌─────────┐
     │security││openapi││telemetry│
     └────────┘└───────┘└─────────┘
```

External runtime dependencies: `pgx` (sql), `grpc-go` (grpc), `opentelemetry` (telemetry).

## Testing

350+ tests across 18 packages. Each package has colocated `*_test.go` files using the stdlib `testing` package.

```bash
# Run all Go framework tests
bunx putnami test --tag go

# Run tests for a specific package
cd go/framework/http && go test ./...
```

## Known Limitations

- **S3 storage**: The lightweight HTTP backend does not implement `List`. Use an AWS SDK-backed implementation for production listing.
- **Event handlers**: The memory broker dispatches handlers in goroutines. Handler code must be goroutine-safe when accessing shared state.
