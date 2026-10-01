# gRPC & Connect

The `grpc` package provides a gRPC server plugin and Connect protocol gateway for the Putnami Go framework.

## gRPC Server Plugin

`Plugin` manages gRPC service registration, interceptor composition, and server lifecycle:

```go
import (
    pgrpc "go.putnami.dev/grpc"
    "google.golang.org/grpc"
)

grpcPlugin := pgrpc.NewPlugin(pgrpc.Config{Port: 9090}).
    WithUnaryInterceptor(pgrpc.LoggingInterceptor(nil)).
    WithUnaryInterceptor(pgrpc.RecoveryInterceptor(nil)).
    WithReflection(true)

// Register your generated service implementations
grpcPlugin.Register(func(s *grpc.Server) {
    pb.RegisterUserServiceServer(s, &userService{})
    pb.RegisterOrderServiceServer(s, &orderService{})
})

// Add to your application
a.Module.Use(grpcPlugin)
```

### Configuration

| Field | Type | Default | Env | Description |
|-------|------|---------|-----|-------------|
| `Port` | `int` | `9090` | `GRPC_PORT` | gRPC server port |
| `MaxConnectionIdle` | `time.Duration` | `15m` | — | Close a connection after this long with no active RPCs |
| `MaxConnectionAge` | `time.Duration` | `30m` | — | Maximum connection lifetime before forcing a reconnect |
| `MaxConnectionAgeGrace` | `time.Duration` | `5m` | — | Extra time for in-flight RPCs after `MaxConnectionAge` |
| `KeepaliveTime` | `time.Duration` | `2m` | — | Idle time before the server pings the client |
| `KeepaliveTimeout` | `time.Duration` | `20s` | — | How long to wait for a keepalive ping ack |
| `MinClientPingInterval` | `time.Duration` | `30s` | — | Minimum tolerated interval between client pings (faster → GOAWAY) |
| `MaxConcurrentStreams` | `uint32` | `1000` | — | Cap on concurrent streams per connection |

Each keepalive/limit field is optional and falls back to a conservative, secure-by-default value when left zero, mirroring the HTTP server's default timeouts. An explicit `WithServerOption(grpc.KeepaliveParams(...))` overrides the corresponding default.

### Service Registration

Register gRPC services using `Register()` with a function that receives the `*grpc.Server`:

```go
grpcPlugin.Register(func(s *grpc.Server) {
    pb.RegisterMyServiceServer(s, &myServiceImpl{})
})
```

### Built-in Interceptors

#### LoggingInterceptor

Logs each RPC call method and errors:

```go
grpcPlugin.WithUnaryInterceptor(pgrpc.LoggingInterceptor(myLogger))
```

#### RecoveryInterceptor

Catches panics in RPC handlers and returns an error instead of crashing:

```go
grpcPlugin.WithUnaryInterceptor(pgrpc.RecoveryInterceptor(myLogger))
```

#### DIInterceptor

Creates a DI scope per RPC request, attaching it to the context:

```go
grpcPlugin.WithUnaryInterceptor(pgrpc.DIInterceptor(containerContext))
```

### Custom Interceptors

Add custom unary or stream interceptors:

```go
grpcPlugin.
    WithUnaryInterceptor(myUnaryInterceptor).
    WithStreamInterceptor(myStreamInterceptor).
    WithServerOption(grpc.MaxRecvMsgSize(4 * 1024 * 1024))
```

### Server Reflection

Server reflection is disabled by default. Enable it for tools like `grpcurl` and `grpcui` in trusted environments:

```go
grpcPlugin.WithReflection(true)
```

## Connect Protocol Gateway

`GatewayPlugin` bridges HTTP and gRPC using the [Connect protocol](https://connectrpc.com/). This allows gRPC services to be called over HTTP/1.1 with JSON or proto encoding:

```go
import (
    pgrpc "go.putnami.dev/grpc"
)

gateway := pgrpc.NewGatewayPlugin(pgrpc.GatewayConfig{})

// Mount Connect handlers (from generated connect code)
path, handler := userconnect.NewUserServiceHandler(&userService{})
gateway.MountHandler(path, handler)

// Register on the HTTP server
// RegisterOn mounts the handlers on the HTTP server and is REQUIRED for the
// services to be reachable.
gateway.RegisterOn(httpServer)

// Optionally also add it as a plugin so it participates in lifecycle logging.
// This does NOT mount handlers on its own — RegisterOn above is what makes the
// services reachable.
a.Module.Use(gateway)
```

### How Connect Works

Connect services are accessible over standard HTTP:

```bash
# JSON over HTTP/1.1
curl -X POST https://api.example.com/package.UserService/GetUser \
  -H "Content-Type: application/json" \
  -d '{"id": "123"}'

# Also supports gRPC and gRPC-Web protocols
```

### Combined Setup

Run both native gRPC (for internal services) and Connect (for web/mobile clients):

```go
// Native gRPC on port 9090
grpcPlugin := pgrpc.NewPlugin(pgrpc.Config{Port: 9090})
grpcPlugin.Register(func(s *grpc.Server) {
    pb.RegisterUserServiceServer(s, svc)
})

// Connect on HTTP server (port 8080)
gateway := pgrpc.NewGatewayPlugin(pgrpc.GatewayConfig{})
path, handler := userconnect.NewUserServiceHandler(svc)
gateway.MountHandler(path, handler)
gateway.RegisterOn(httpServer)

a.Module.Use(grpcPlugin)
a.Module.Use(gateway)
```

## Support and contract

The SDD owner is `go`. `go.putnami.dev/grpc` is public, documented, maintained, and
classified `stable` in the workspace [support catalog](../../../putnami.support.json).
Before v1.0.0, a minor `0.x` release may still contain a breaking change; the
[release policy](../../../RELEASE.md) requires release notes and migration
documentation rather than strict compatibility between every pre-1.0 minor.

The [gRPC services specification](specs/grpc-services.json) defines the contract,
backed by two durable decisions: [the Connect bridge re-enters the endpoint
pipeline and is served from the published
descriptor](doc/adr/0001-bridge-reenters-the-endpoint-pipeline.md), and [bound
the server by default, mount the gateway
explicitly](doc/adr/0002-bounded-by-default-mounted-explicitly.md).

`ApiBridge` speaks Connect protocol version 1 in four shapes: a unary JSON body,
a unary protobuf body, and a declared server stream in either codec as 5-byte
prefixed envelopes ended by exactly one `EndStreamResponse`. The two protobuf
shapes exist only when the [proto plugin](../proto) publishes a descriptor —
that descriptor *is* the codec, so no stub is generated and none is needed. A
provider without it serves and advertises JSON alone. Client and bidirectional
streams stay outside this contract: Connect carries at most one request message
per call.

`Connect-Timeout-Ms` bounds the context the endpoint runs under, gzip is
negotiated in both directions and bounded in the decompressed one, and every
decoded message is bounded by `WithMaxMessageBytes` before it is allocated for.
A failure becomes the Connect error document: one of the specification's sixteen
codes, with the stable first-party code, the exact HTTP status and any declared
`details` body in a `putnami.client.v1.FrameworkError` detail — the same message
the TypeScript provider publishes, so a Putnami consumer reads one encoding
whichever provider answered.

When the proto plugin has published its descriptor, the bridge mounts each route
at the method identity that descriptor declares instead of recomputing an RPC
name, and `Start` refuses to run if a mounted URL is not the declared method — so
a plugin registration order cannot produce a contract naming methods no URL
answers. A mounted bridge publishes the payload encodings it serves back to the
api plugin, which is what lets a first-party contract advertise a Connect
transport at all: the descriptor says the method exists, the bridge says a server
answers it, and only both together publish it.

Regression evidence covers [plugin lifecycle, interceptors, and the
gateway](grpc_test.go), [the end-to-end interceptor
chain](integration_test.go), [the api → Connect bridge](api_bridge_test.go),
[the Connect protocol the bridge serves](connect_bridge_test.go), [the wire
vectors both codecs are pinned to](../../../protocols/clientcontract/connect/oracle_test.go), [the shared Connect
conformance corpus](../../../protocols/clientcontract/connect/corpus_test.go), [a generated Go client calling a
real Go provider over Connect](connect_interop_test.go),
[keepalive and stream defaults](keepalive_test.go), [bounded
shutdown](shutdown_test.go), [stream interceptors and gateway
mounting](stream_gateway_test.go), and [concurrent RPC handling](concurrent_test.go).
