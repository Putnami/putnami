# Go Simple API Example

The smallest useful Putnami Go service: an HTTP server, three middleware, four
routes, and a health endpoint. No dependency injection, no events, no
configuration loading — start here, then read
[`task-api`](../task-api) for the fuller composition and
[`service-to-service`](../service-to-service) for typed clients.

Modules used:

- [`app`](../../framework/app) — application lifecycle and plugin orchestration
- [`http`](../../framework/http) — server, router, middleware, responses, and
  the single-endpoint health plugin
- [`logger`](../../framework/logger) — structured logging

## Running

```bash
# From the workspace root
putnami serve go.putnami.dev/examples/simple-api

# Or directly
cd go/samples/simple-api && go run .
```

`putnami serve` binds port 3802 through the `PORT` environment variable;
`ServerConfig{Port: 8080}` is the fallback a direct `go run .` uses.

```bash
curl -s localhost:3802/_/health
curl -s -XPOST localhost:3802/greetings -d '{"message":"hello"}'
curl -s localhost:3802/greetings
curl -s localhost:3802/greetings/greet-1
curl -s -XDELETE localhost:3802/greetings/greet-1 -o /dev/null -w '%{http_code}\n'
```

## API

| Method   | Path                | Description            |
|----------|---------------------|------------------------|
| `GET`    | `/greetings`        | List all greetings     |
| `POST`   | `/greetings`        | Create a greeting      |
| `GET`    | `/greetings/{id}`   | Get a greeting by ID   |
| `DELETE` | `/greetings/{id}`   | Delete a greeting      |
| `GET`    | `/_/health`         | Liveness (`http.NewHealthPlugin`) |

`HEAD` falls back to the `GET` handler, which is why the described route
inventory lists it alongside `GET`.

## Middleware

```go
server.Use(http.Recovery())   // a handler panic becomes 500, not a crash
server.Use(http.RequestID())  // X-Request-ID in, propagated out, bridged into logs
server.Use(http.Logging(http.LoggerOptions{
    Exclude: []string{"/_/health"}, // keep the probe out of the access log
}))
```

Middleware runs in registration order around every route and is composed once,
before the first request.

## Health plugin versus platform endpoints

`a.Use(http.NewHealthPlugin())` mounts one liveness route at `/_/health` on the
application's server. A service that
needs the full operational surface — `/livez`, `/healthz`, `/readyz`, `/version` —
uses [`go.putnami.dev/platform`](../../framework/platform) instead, as
[`task-api`](../task-api) does. Both can share one server, as
[`migrations-feature`](../migrations-feature) shows: each discovers the same
probes and reports them on its own route.

## Verifying

```bash
putnami lint,test,build --projects go.putnami.dev/examples/simple-api
```

`capabilities_conformance_test.go` additionally certifies that the committed
`schema/capabilities.json` is byte-canonical and scoped to this project.
