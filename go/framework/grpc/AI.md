# go.putnami.dev/grpc

gRPC server plugin with service registration, interceptors, and Connect protocol gateway.

## Quick Start

```go
import (
	"go.putnami.dev/app"
	pgrpc "go.putnami.dev/grpc"

	"google.golang.org/grpc"
)

grpcPlugin := pgrpc.NewPlugin(pgrpc.Config{Port: 9090})
grpcPlugin.Register(func(s *grpc.Server) {
	pb.RegisterUserServiceServer(s, &userServiceImpl{})
})

a := app.New("my-service")
a.Use(grpcPlugin)
a.ListenAndServe()
```

The Putnami package is aliased to `pgrpc` so the registrar callback can refer to
`*grpc.Server` from `google.golang.org/grpc`.

## Built-in Interceptors

Interceptors are added with `WithUnaryInterceptor`; the built-ins take a
`*logger.Logger` (pass `nil` for the default):

```go
grpcPlugin := pgrpc.NewPlugin(pgrpc.Config{Port: 9090}).
	WithUnaryInterceptor(pgrpc.LoggingInterceptor(nil)).
	WithUnaryInterceptor(pgrpc.RecoveryInterceptor(nil)).
	WithUnaryInterceptor(pgrpc.DIInterceptor(containerContext))
```

Unary and stream interceptors are separate in grpc-go, so streaming RPCs need
the stream variants (registered with `WithStreamInterceptor`). Notably,
`RecoveryStreamInterceptor` is required to keep a panic in a streaming handler
from crashing the server:

```go
grpcPlugin.
	WithStreamInterceptor(pgrpc.RecoveryStreamInterceptor(nil)).
	WithStreamInterceptor(pgrpc.LoggingStreamInterceptor(nil)).
	WithStreamInterceptor(pgrpc.DIStreamInterceptor(containerContext))
```

DI scoping is wired explicitly (as above); the plugin does not capture a
container in `Configure`.

## Server Reflection

Reflection is disabled by default. Enable it for tools like `grpcurl`/`grpcui`:

```go
grpcPlugin.WithReflection(true)
```

## Connect Protocol Gateway

Expose gRPC services over HTTP/JSON via the Connect protocol:

```go
gateway := pgrpc.NewGatewayPlugin(pgrpc.GatewayConfig{})

// Mount a Connect handler (from generated connect code)
path, handler := userconnect.NewUserServiceHandler(&userServiceImpl{})
gateway.MountHandler(path, handler)

// Attach to an existing HTTP server plugin. RegisterOn is REQUIRED for the
// services to be reachable; adding the gateway via Use() alone mounts nothing.
gateway.RegisterOn(httpServer)
```

## api → Connect bridge

`pgrpc.NewApiBridge(apiPlugin, server)` mounts every bound api endpoint at a
Connect-style URL. Register it **after** the proto plugin: it then takes each URL
from `apiPlugin.ClientProtobufMethods()` (published by the proto plugin) rather
than recomputing an RPC name, and `Start` fails when a mounted URL is not the
method identity the descriptor declares. A mounted bridge publishes the payload
encodings it actually serves back to the api plugin, which is what makes a
first-party contract advertise a Connect transport at all.

### What the bridge serves

| Content-Type | Shape |
|---|---|
| `application/json` (or absent) | unary, the `{params, query, body}` JSON envelope |
| `application/proto` | unary, the same envelope in protobuf binary |
| `application/connect+json` | declared server stream, enveloped JSON messages |
| `application/connect+proto` | declared server stream, enveloped protobuf messages |

The two protobuf shapes are served **only** when the api plugin publishes a
protobuf descriptor: that descriptor is the codec, and no stub is generated. A
provider with no proto plugin publishes `[json]` and serves JSON alone. Client and
bidirectional streams get no Connect URL — Connect carries at most one request
message per call.

```go
bridge := pgrpc.NewApiBridge(apiPlugin, httpServer,
    pgrpc.WithPackage("widgets.v1"),
    pgrpc.WithMaxMessageBytes(4<<20),        // bounds one decoded message, gzip included
    pgrpc.WithStreamWriteTimeout(30*time.Second), // bounds one stream write, not the stream
)
```

`Connect-Timeout-Ms` bounds the context the endpoint runs under; a malformed
header is refused, never ignored. gzip is negotiated in both directions and
bounded in the decompressed one. A failure becomes the Connect error document:
one of the specification's sixteen codes, with the stable first-party code, the
exact HTTP status and any declared `details` body in a
`putnami.client.v1.FrameworkError` detail — the message the TypeScript provider
publishes too.

See `doc/getting-started.md` for full reference, and
[ADR 0001](doc/adr/0001-bridge-reenters-the-endpoint-pipeline.md) for
what is derived from the descriptor and the two deliberate deviations from the
specification.

## Maintained contract

This public package is `stable` in the workspace [support
catalog](../../../putnami.support.json). Its behavior is defined by the [gRPC
services specification](specs/grpc-services.json), the [bridge
ADR](doc/adr/0001-bridge-reenters-the-endpoint-pipeline.md), and the
[bounded-server ADR](doc/adr/0002-bounded-by-default-mounted-explicitly.md).
Before v1.0, follow the workspace [migration-based compatibility
policy](../../../RELEASE.md); do not infer strict compatibility between every
`0.x` minor.
