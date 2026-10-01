# gRPC & Connect

`go.putnami.dev/grpc` provides a gRPC server plugin and a Connect protocol gateway for serving gRPC over HTTP/1.1.

## gRPC server plugin

### Basic setup

```go
import (
    "go.putnami.dev/app"
    "go.putnami.dev/grpc"
)

plugin := grpc.NewPlugin(grpc.Config{
    Port: 9090, // env: GRPC_PORT
})

// Register gRPC services
plugin.Register(func(s *grpc.Server) {
    pb.RegisterUserServiceServer(s, &userServer{})
    pb.RegisterOrderServiceServer(s, &orderServer{})
})

a := app.New("my-service")
a.Module.Use(plugin)
a.ListenAndServe()
```

### Configuration

| Field | Type | Default | Env var | Description |
|-------|------|---------|---------|-------------|
| `Port` | `int` | `9090` | `GRPC_PORT` | gRPC server port |

### Server options

```go
plugin := grpc.NewPlugin(grpc.Config{Port: 9090})

// Add interceptors
plugin.WithUnaryInterceptor(myInterceptor)
plugin.WithStreamInterceptor(myStreamInterceptor)

// Enable server reflection (for grpcurl, etc.)
plugin.WithReflection(true)

// Pass raw grpc.ServerOption
plugin.WithServerOption(grpc.MaxRecvMsgSize(10 << 20))
```

## Built-in interceptors

### Logging

Logs each RPC call with method, duration, and status:

```go
import "go.putnami.dev/logger"

plugin.WithUnaryInterceptor(grpc.LoggingInterceptor(logger.Default()))
```

### Recovery

Catches panics in handlers and returns an `Internal` error:

```go
plugin.WithUnaryInterceptor(grpc.RecoveryInterceptor(logger.Default()))
```

### DI scoping

Creates a DI scope per gRPC request, enabling per-request service resolution:

```go
import "go.putnami.dev/inject"

plugin.WithUnaryInterceptor(grpc.DIInterceptor(containerContext))
```

## Connect protocol gateway

The Connect gateway exposes gRPC services over HTTP/1.1 with JSON encoding, making them accessible from browsers and REST clients.

### Setup

```go
import (
    fhttp "go.putnami.dev/http"
    "go.putnami.dev/grpc"
)

server := fhttp.NewServerPlugin(fhttp.ServerConfig{Port: 3000})
gateway := grpc.NewGatewayPlugin(grpc.GatewayConfig{})

// Mount Connect handlers
gateway.MountHandler("/users.v1.UserService/", userConnectHandler)

// Register on the HTTP server
gateway.RegisterOn(server)

a := app.New("my-service")
a.Module.Use(server)
a.Module.Use(gateway)
```

### Calling Connect endpoints

Connect endpoints are accessible as standard HTTP POST requests:

```
POST /users.v1.UserService/GetUser
Content-Type: application/json

{"id": "user-123"}
```

Response:

```json
{"name": "Jane", "email": "jane@example.com"}
```

## Lifecycle

Both the gRPC plugin and Connect gateway implement the standard lifecycle interfaces:

- **Configure** — register services, configure interceptors
- **Start** — begin listening for connections
- **Stop** — graceful shutdown with drain

```
Application.Start()
    → gRPC.Configure()     — register services
    → gRPC.Start()      — listen on :9090
    → Gateway.Configure()  — mount handlers
    → Gateway.Start()   — ready for HTTP traffic

Application.Stop()
    → gRPC.Stop()       — graceful stop
```

## Protobuf contracts

`go.putnami.dev/proto` renders a `.proto` service definition from the same
`api.Plugin` route metadata the OpenAPI document is generated from:

```go
apiPlugin := api.New(httpServer)
protoPlugin := proto.NewPlugin(proto.PluginOptions{
    PackageName: "myapp.v1",
    GoPackage:   "example.com/myapp/proto/v1",
}).From(apiPlugin)

a.Use(httpServer).Use(apiPlugin).Use(protoPlugin)
```

`putnami build` writes the document to `schema/api.proto`. The plugin writes
nothing while the service is starting or running; to export it outside a Putnami
build, call `protoPlugin.WriteTo("")` after the configure phase.

RPC names are derived from method and path (`GET /users` → `ListUsers`,
`POST /users` → `CreateUsers`), and the api → Connect bridge derives its URLs the
same way, so a generated client and the served route agree.

## Bridging api endpoints to Connect

`grpc.NewApiBridge` exposes every bound `api.Endpoint` at
`POST /<package>.<Service>/<RPCName>` by wrapping the endpoint's own handler, so
validation, security, and error shape are identical to a direct REST call.

The request body is a namespaced envelope, which keeps identically named fields
from colliding:

```json
{ "params": { "id": "42" }, "query": { "expand": "true" }, "body": { "id": "renamed" } }
```

The bridge carries JSON only — Protobuf binary encoding and streaming methods are
outside its contract — and endpoints finalized with `Document()` are skipped,
since their handlers are mounted elsewhere.

## Related guides

- [HTTP & Middleware](/docs/frameworks/go/http) — HTTP server for Connect gateway
- [Plugins & Lifecycle](/docs/frameworks/go/plugins-and-lifecycle) — plugin lifecycle
- [Dependency Injection](/docs/frameworks/go/dependency-injection) — per-request DI scoping
## Support and contract

`go.putnami.dev/grpc` and `go.putnami.dev/proto` are `stable` in the workspace
support catalog. gRPC behavior is defined by the gRPC services specification and
two accepted decision records under `go/framework/grpc/`; the Protobuf document
is one half of the API contracts specification under `go/framework/api/specs/`,
with its own decision record under `go/framework/proto/doc/adr/`. Before v1.0.0 a
minor `0.x` release may still contain a documented breaking change; strict
compatibility between every pre-1.0 minor is not promised.
