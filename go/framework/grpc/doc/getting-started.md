# gRPC Server

`go.putnami.dev/grpc` provides a gRPC server plugin for the Putnami Go framework. It integrates with the application lifecycle and DI container, supporting both native gRPC transport and an HTTP gateway via the Connect protocol. The module depends on `google.golang.org/grpc` and several Putnami core modules (`app`, `inject`, `logger`, `errors`, `http`).

## Setting Up the gRPC Server

Create a `Plugin` with a `Config` struct that specifies the listening port. Register it on an application module like any other plugin.

```go
import (
    "go.putnami.dev/app"
    "go.putnami.dev/grpc"
)

func main() {
    grpcPlugin := grpc.NewPlugin(grpc.Config{
        Port: 9090,
    })

    app.New("my-app").
        Use(grpcPlugin).
        ListenAndServe()
}
```

The `Config` struct supports the following fields:

| Field | Type | Default | Env var | Description |
|---|---|---|---|---|
| `Port` | `int` | `9090` | `GRPC_PORT` | TCP port the gRPC server listens on |
| `MaxConnectionIdle` | `time.Duration` | `15m` | — | Close a connection after this long with no active RPCs |
| `MaxConnectionAge` | `time.Duration` | `30m` | — | Maximum connection lifetime before forcing a reconnect |
| `MaxConnectionAgeGrace` | `time.Duration` | `5m` | — | Extra time for in-flight RPCs after `MaxConnectionAge` |
| `KeepaliveTime` | `time.Duration` | `2m` | — | Idle time before the server pings the client |
| `KeepaliveTimeout` | `time.Duration` | `20s` | — | How long to wait for a keepalive ping ack |
| `MinClientPingInterval` | `time.Duration` | `30s` | — | Minimum tolerated interval between client pings (faster → GOAWAY) |
| `MaxConcurrentStreams` | `uint32` | `1000` | — | Cap on concurrent streams per connection |
| `ShutdownTimeout` | `time.Duration` | `10s` | — | Bounds the graceful drain on `Stop` before the server is force-closed |

The keepalive and limit fields are optional: each falls back to a conservative default when left zero, so the server enforces connection-lifetime, keepalive, and per-connection stream limits out of the box (the same secure-by-default posture as the HTTP server's timeouts). Supply a `WithServerOption(grpc.KeepaliveParams(...))` to override.

The server starts during the plugin `Start` phase and shuts down gracefully during `Stop`. `Stop` runs `GracefulStop` in the background and, if the drain has not finished by `ShutdownTimeout` (or the shutdown context's deadline, whichever is sooner), force-closes the server so a stuck or long-lived stream cannot hang shutdown indefinitely. This mirrors the HTTP server's bounded drain.

## Registering Services

Use `Register` to attach gRPC service implementations to the server. Each registrar is a function that receives the underlying `*grpc.Server` and calls the generated `Register*Server` function from your protobuf package.

```go
import (
    "go.putnami.dev/grpc"
    pb "myapp/proto/userpb"
    "google.golang.org/grpc"
)

grpcPlugin := grpc.NewPlugin(grpc.Config{Port: 9090}).
    Register(func(s *grpc.Server) {
        pb.RegisterUserServiceServer(s, &userServiceImpl{})
    }).
    Register(func(s *grpc.Server) {
        pb.RegisterOrderServiceServer(s, &orderServiceImpl{})
    })
```

Multiple services can be registered by chaining `Register` calls. All registrars run during the `Start` phase, before the server begins accepting connections.

## Interceptors

Interceptors run before each RPC handler, enabling cross-cutting concerns like logging, error recovery, and dependency injection. The plugin supports both unary and stream interceptors.

### Adding Interceptors

```go
grpcPlugin := grpc.NewPlugin(grpc.Config{Port: 9090}).
    WithUnaryInterceptor(myUnaryInterceptor).
    WithStreamInterceptor(myStreamInterceptor)
```

When multiple interceptors of the same kind are added, they are chained in registration order using `grpc.ChainUnaryInterceptor` and `grpc.ChainStreamInterceptor`.

### Built-in Interceptors

The package provides three ready-to-use unary interceptors.

#### LoggingInterceptor

Logs each RPC call at debug level on success and error level on failure.

```go
grpcPlugin.WithUnaryInterceptor(grpc.LoggingInterceptor(nil))
```

Pass `nil` to use a default logger named `"grpc"`, or provide your own `*logger.Logger`.

#### RecoveryInterceptor

Catches panics inside **unary** RPC handlers and converts them to gRPC errors with an internal error code. Without it, a panic in a unary handler would crash the entire server process.

```go
grpcPlugin.WithUnaryInterceptor(grpc.RecoveryInterceptor(nil))
```

This interceptor only runs for unary RPCs. Streaming handlers need the stream
variant — see [Stream Interceptors](#stream-interceptors) below.

#### DIInterceptor

Creates a dependency injection scope for each request. Services resolved from the scoped container are isolated per-request and automatically cleaned up when the RPC completes.

```go
grpcPlugin.WithUnaryInterceptor(grpc.DIInterceptor(containerContext))
```

If the container context is `nil`, the interceptor passes through without creating a scope.

#### Stream Interceptors

grpc-go runs unary and stream interceptors separately, so the unary interceptors above do **not** apply to streaming RPCs. Each has a stream counterpart with the same behavior, registered with `WithStreamInterceptor`:

```go
grpcPlugin.
    WithStreamInterceptor(grpc.RecoveryStreamInterceptor(nil)).
    WithStreamInterceptor(grpc.LoggingStreamInterceptor(nil)).
    WithStreamInterceptor(grpc.DIStreamInterceptor(containerContext))
```

`RecoveryStreamInterceptor` is the important one: without it a panic in a streaming handler crashes the server process, exactly as it would for an unprotected unary handler.

### Writing a Custom Interceptor

A unary interceptor is a function matching the `grpc.UnaryServerInterceptor` signature. Call `handler` to proceed to the next interceptor or the final handler.

```go
func authInterceptor(
    ctx context.Context,
    req any,
    info *grpc.UnaryServerInfo,
    handler grpc.UnaryHandler,
) (any, error) {
    // Extract metadata, validate token, etc.
    token := extractToken(ctx)
    if token == "" {
        return nil, status.Error(codes.Unauthenticated, "missing token")
    }
    return handler(ctx, req)
}
```

### Recommended Interceptor Order

Register interceptors in this order so that recovery catches panics from all subsequent interceptors, logging observes the final result, and DI scoping is available to the handler:

```go
grpcPlugin := grpc.NewPlugin(grpc.Config{Port: 9090}).
    WithUnaryInterceptor(grpc.RecoveryInterceptor(nil)).
    WithUnaryInterceptor(grpc.LoggingInterceptor(nil)).
    WithUnaryInterceptor(grpc.DIInterceptor(cc))
```

## Server Options

Pass additional `grpc.ServerOption` values for low-level server tuning.

```go
grpcPlugin := grpc.NewPlugin(grpc.Config{Port: 9090}).
    WithServerOption(grpc.MaxRecvMsgSize(4 * 1024 * 1024)).
    WithServerOption(grpc.MaxSendMsgSize(4 * 1024 * 1024))
```

## Reflection

Server reflection is disabled by default. Enable it so tools like `grpcurl` and `grpcui` can discover services at runtime — typically only in development or other trusted environments, since it exposes the full service schema to any client:

```go
grpcPlugin := grpc.NewPlugin(grpc.Config{Port: 9090}).
    WithReflection(true)
```

## HTTP Gateway (Connect Protocol)

The `GatewayPlugin` bridges HTTP and gRPC using the Connect protocol. It mounts Connect service handlers on the Putnami HTTP server, allowing gRPC services to be called over HTTP/1.1 with JSON or protobuf encoding.

### Setting Up the Gateway

```go
import (
    "go.putnami.dev/grpc"
    phttp "go.putnami.dev/http"
    "myapp/proto/userconnect" // generated Connect code
)

httpPlugin := phttp.NewServerPlugin(phttp.ServerConfig{Port: 8080})
gateway := grpc.NewGatewayPlugin(grpc.GatewayConfig{})

// Mount a Connect service handler
path, handler := userconnect.NewUserServiceHandler(userServiceImpl)
gateway.MountHandler(path, handler)

// Register the gateway routes on the HTTP server
gateway.RegisterOn(httpPlugin)

module := app.NewModule("my-app").
    Use(httpPlugin).
    Use(gateway)
```

The `GatewayConfig` struct supports the following fields:

| Field | Type | Default | Description |
|---|---|---|---|
| `PathPrefix` | `string` | `"/"` | Base HTTP path prefix for Connect handlers. Normalized like every route prefix: whitespace and slashes at either end are dropped, so `"api"`, `" api"`, `"//api"` and `"/api/"` all mount under `/api` |

### Mounting Services

There are two ways to mount a Connect service:

```go
// Option 1: MountHandler with path and handler separately
path, handler := userconnect.NewUserServiceHandler(svc)
gateway.MountHandler(path, handler)

// Option 2: Mount with a ServiceHandler struct
gateway.Mount(grpc.ServiceHandler{
    Path:    "/package.UserService/",
    Handler: connectHandler,
})
```

The gateway registers each service as a `POST` wildcard route on the HTTP server. Connect clients can then call services using standard HTTP requests.

## Accessing the Underlying Server

After the plugin has started, use `Server()` to access the underlying `*grpc.Server` instance. This returns `nil` before the server starts.

```go
srv := grpcPlugin.Server()
```

## Error Codes

The package defines the following structured error codes:

| Code | Description |
|---|---|
| `grpc.listen` | Failed to bind the TCP listener |
| `grpc.scope` | Failed to create or close a DI scope |
| `grpc.panic` | A panic was recovered inside an RPC handler |

## Best Practices

- **Always register both recovery interceptors.** Register `RecoveryInterceptor` (unary) *and* `RecoveryStreamInterceptor` (stream): a panic in any handler crashes the server process without the matching interceptor, and the unary one does not cover streaming RPCs.
- **Use `DIInterceptor` for per-request services**. This ensures each RPC gets its own scoped dependencies (database connections, transactions) that are automatically cleaned up.
- **Keep reflection disabled in production.** It is off by default; only enable it (`WithReflection(true)`) in trusted environments, since it exposes the full service schema to any client.
- **Prefer the Connect gateway over raw gRPC** when clients are web browsers or need HTTP/1.1 compatibility. The gateway allows JSON encoding and works through standard HTTP proxies.
- **Chain interceptors in a deliberate order**. Recovery should be outermost, logging next, then DI scoping, then authentication.
- **Use `WithServerOption` for transport tuning** such as max message sizes, keepalive settings, and TLS credentials.

## Contract and compatibility

See the [gRPC services specification](../specs/grpc-services.json), the [bridge
ADR](adr/0001-bridge-reenters-the-endpoint-pipeline.md), the [bounded-server
ADR](adr/0002-bounded-by-default-mounted-explicitly.md), and [support
evidence](../README.md#support-and-contract). The package is stable and
maintained, with the workspace's [pre-1.0 migration
policy](../../../../RELEASE.md), not strict compatibility between every `0.x`
minor.
