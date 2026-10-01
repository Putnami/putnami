# Go Simple API Example

The smallest useful Putnami Go service: an HTTP server, three middleware, four
routes, and the operational endpoints. No dependency injection, no events, no
configuration loading — start here, then read
[`task-api`](../task-api) for the fuller composition and
[`service-to-service`](../service-to-service) for typed clients.

Modules used:

- [`app`](../../framework/app) — application lifecycle and plugin orchestration
- [`http`](../../framework/http) — server, router, middleware, responses, and
  the single-endpoint health plugin
- [`logger`](../../framework/logger) — structured logging
- [`platform`](../../framework/platform) — `/livez`, `/healthz`, `/readyz` and
  `/version`

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
curl -s localhost:3802/readyz
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
| `GET`    | `/livez`, `/healthz`, `/readyz`, `/version` | Operational endpoints (`platform.NewPlugin`) |

`HEAD` falls back to the `GET` handler, which is why the described route
inventory lists it alongside `GET`.

## Middleware

```go
server.Use(http.Recovery())   // a handler panic becomes 500, not a crash
server.Use(http.RequestID())  // X-Request-ID in, propagated out, bridged into logs
server.Use(http.Logging(http.LoggerOptions{
    Exclude: []string{"/_/health", "/livez", "/healthz", "/readyz"}, // keep probes out of the access log
}))
```

Middleware runs in registration order around every route and is composed once,
before the first request.

## Health plugin versus platform endpoints

`a.Use(http.NewHealthPlugin())` mounts one liveness route at `/_/health` on the
application's server. `a.Use(platform.NewPlugin(platform.Config{}))` mounts the
full operational surface — `/livez`, `/healthz`, `/readyz`, `/version` — on the
same server. This sample composes both: each discovers the same probes and
reports them on its own route. `putnami qualify` and deployment probes wait on
`/readyz`, so a service needs the platform plugin to be qualified; the health
plugin serves clients that probe `/_/health`.

## Verifying

```bash
putnami lint,test,build --projects go.putnami.dev/examples/simple-api
```

`capabilities_conformance_test.go` additionally certifies that the committed
`schema/capabilities.json` is byte-canonical and scoped to this project.
