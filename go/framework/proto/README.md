# go.putnami.dev/proto

Generates Protocol Buffer definitions (`.proto`) from `api.Plugin` route metadata.
Each endpoint becomes one RPC on a single `ApiService`; request/response messages
are derived from the endpoint's `Params`, `Query`, `Body`, and `Returns` types.

## Usage

```go
import (
    "go.putnami.dev/api"
    "go.putnami.dev/app"
    "go.putnami.dev/http"
    "go.putnami.dev/proto"
)

httpServer := http.NewServerPlugin(http.ServerConfig{Port: 8080})
apiPlugin := api.New(httpServer)
apiPlugin.Register(...)

protoPlugin := proto.NewPlugin(proto.PluginOptions{
    PackageName: "myapp.v1",
    GoPackage:   "example.com/myapp/proto/v1",
}).From(apiPlugin)

app.New("myapp").Use(httpServer).Use(apiPlugin).Use(protoPlugin)
```

The default output path is `schema/api.proto` (committed). Set
`PluginOptions.Output = proto.FallbackOutputPath` to keep the artifact in
`.gen/` instead.

## RPC naming

| HTTP                | RPC name             |
|---------------------|----------------------|
| `GET    /users`     | `ListUsers`          |
| `GET    /users/{id}`| `GetUsers`           |
| `POST   /users`     | `CreateUsers`        |
| `PUT    /users/{id}`| `UpdateUsers`        |
| `DELETE /users/{id}`| `DeleteUsers`        |

Path segments are camel-cased and joined; path parameters are dropped from the
verb derivation but kept on the request message.

## What the descriptor carries

The rendered `.proto` and the descriptor published to the first-party client
contract state the same wire shape the OpenAPI document states: each field's
exact number, its kind, explicit proto3 presence for anything the schema lets be
absent or null, a map's exact key and value type, and integer widths as
declared. `int` is 64 bits on every transport; a 32-bit field is declared with a
32-bit Go type.

A declaration proto3 cannot carry — a map keyed by something other than a
string, a nested list, a map of lists — fails generation with the remedy named,
and the document does not render. See
`doc/adr/0002-a-descriptor-states-the-wire-shape-the-schema-states.md`.

## Which routes become RPCs

Every route the api plugin binds to a transport becomes one RPC. Routes finalized
with `api.Endpoint(...).Document()` are skipped: their handler is mounted directly
on the server, outside the api builder, so there is nothing for the Connect bridge
to invoke and an emitted RPC would advertise a URL that cannot answer. Those routes
are still served and still appear in the OpenAPI document.

## Producing the artifact

`Configure` renders the document into memory and writes nothing. Build-time
describe — `putnami build` — writes `schema/api.proto` below the describe output
directory, and the runner copies it into the project tree.

To export the document outside a Putnami build, call `WriteTo`:

```go
if err := protoPlugin.WriteTo(""); err != nil { // "" uses PluginOptions.Output
    return err
}
```

A workload that relied on the previous behavior — `Configure` writing the file
into the process working directory at every application start — regenerates the
artifact with `putnami build`, or calls `WriteTo("")` after `Configure`. This is
a documented pre-1.0 breaking change under the
[release policy](../../../RELEASE.md); the reasoning is in
[ADR 0001](doc/adr/0001-contract-artifacts-are-build-outputs.md).

## Support and contract

The SDD owner is `go`. `go.putnami.dev/proto` is public, documented, maintained,
and classified `stable` in the workspace [support
catalog](../../../putnami.support.json). Before v1.0.0, a minor `0.x` release may
still contain a breaking change; the [release policy](../../../RELEASE.md) requires
release notes and migration documentation rather than strict compatibility between
every pre-1.0 minor.

This package does not own a feature of its own: it renders one half of the
[`go/api-contracts`](../api/putnami.features.json) outcome, whose
[specification](../api/specs/api-contracts.json) lives with
[`go.putnami.dev/api`](../api). Its own durable decision is [a contract artifact
is a build output](doc/adr/0001-contract-artifacts-are-build-outputs.md).

Regression evidence covers [RPC naming, message derivation, type mapping, and
name disambiguation](proto_test.go), [discovery through the api
plugin](api_integration_test.go), and [describe output, the absence of a startup
write, and the explicit export](describe_test.go).
