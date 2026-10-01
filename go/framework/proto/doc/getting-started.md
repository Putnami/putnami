# Protocol Buffers

`go.putnami.dev/proto` generates a Protocol Buffer definition (`.proto`) from the
routes an `api.Plugin` discovers. Every endpoint becomes one RPC on a single
`ApiService`; request and reply messages are derived from each endpoint's
`Params`, `Query`, `Body`, and `Returns` types. The render is deterministic
(messages in registration order, fields in struct-field order) so the output is
safe to commit and drift can be caught with a diff.

## Wiring It Up

Register the proto plugin alongside the HTTP server and the API plugin, and point
it at the API plugin with `From`. The API plugin must be registered first so its
`Configure` runs before the proto plugin reads the discovered routes.

```go
package main

import (
    "go.putnami.dev/api"
    "go.putnami.dev/app"
    "go.putnami.dev/http"
    "go.putnami.dev/proto"
)

type User struct {
    ID    string `json:"id"`
    Name  string `json:"name"`
    Email string `json:"email"`
}

type UserParams struct {
    ID string `json:"id" validate:"required,uuid"`
}

type CreateUserBody struct {
    Name  string `json:"name" validate:"required"`
    Email string `json:"email" validate:"required,email"`
}

func main() {
    httpServer := http.NewServerPlugin(http.ServerConfig{Port: 8080})

    apiPlugin := api.New(httpServer)
    apiPlugin.Register(api.Endpoint("GET", "/users").
        Description("List users").
        Returns(api.Type[[]User]()).
        Handle(listUsers))
    apiPlugin.Register(api.Endpoint("GET", "/users/{id}").
        Params(api.Type[UserParams]()).
        Returns(api.Type[User]()).
        Handle(getUser))
    apiPlugin.Register(api.Endpoint("POST", "/users").
        Body(api.Type[CreateUserBody]()).
        Returns(api.Type[User]()).
        Handle(createUser))

    protoPlugin := proto.NewPlugin(proto.PluginOptions{
        PackageName: "myapp.v1",
        GoPackage:   "example.com/myapp/proto/v1",
    }).From(apiPlugin)

    app.New("myapp").
        Use(httpServer).
        Use(apiPlugin).
        Use(protoPlugin).
        ListenAndServe()
}

func listUsers(ctx *http.EndpointContext) *http.Response  { return http.JSON([]User{}) }
func getUser(ctx *http.EndpointContext) *http.Response    { return http.JSON(User{}) }
func createUser(ctx *http.EndpointContext) *http.Response { return http.JSON(User{}) }
```

## The Generated `schema/api.proto`

The three endpoints above render to:

```proto
syntax = "proto3";

package myapp.v1;

option go_package = "example.com/myapp/proto/v1";

message ListUsersRequest {
}

message User {
  string id = 1;
  string name = 2;
  string email = 3;
}

message ListUsersReply {
  repeated User value = 1;
}

message GetUsersParams {
  string id = 1;
}

message GetUsersRequest {
  GetUsersParams params = 1;
}

message GetUsersReply {
  string id = 1;
  string name = 2;
  string email = 3;
}

message CreateUsersBody {
  string name = 1;
  string email = 2;
}

message CreateUsersRequest {
  CreateUsersBody body = 1;
}

message CreateUsersReply {
  string id = 1;
  string name = 2;
  string email = 3;
}

service ApiService {
  // List users
  rpc ListUsers(ListUsersRequest) returns (ListUsersReply);
  rpc GetUsers(GetUsersRequest) returns (GetUsersReply);
  rpc CreateUsers(CreateUsersRequest) returns (CreateUsersReply);
}
```

A top-level slice return (`[]User`) becomes a `repeated User value` field; a
struct return is inlined directly into the reply. Path params, query params, and
body fields each get their own nested section message so a path-param `id` and a
body-field `id` never collide.

## Plugin Options

```go
proto.NewPlugin(proto.PluginOptions{
    PackageName: "myapp.v1",                 // proto `package`; default "api.v1"
    GoPackage:   "example.com/myapp/proto/v1", // optional `option go_package`
    Output:      proto.DefaultOutputPath,    // used only by WriteTo("")
})
```

| Field | Default | Purpose |
|-------|---------|---------|
| `PackageName` | `api.v1` | The proto `package` declaration |
| `GoPackage` | `""` | Optional `option go_package = "..."` |
| `Output` | `schema/api.proto` | The path `WriteTo("")` writes to. It does **not** affect `Describe`, which always writes `schema/api.proto` below the runner's output directory, and nothing is ever written during `Configure`. Set it to `proto.FallbackOutputPath` (`.gen/schema/api.proto`) when driving `WriteTo` yourself and you want the artifact out of the reviewed tree |

## RPC Naming

| HTTP                | RPC name      |
|---------------------|---------------|
| `GET    /users`     | `ListUsers`   |
| `GET    /users/{id}`| `GetUsers`    |
| `POST   /users`     | `CreateUsers` |
| `PUT    /users/{id}`| `UpdateUsers` |
| `DELETE /users/{id}`| `DeleteUsers` |

Path segments are camel-cased and joined; path parameters are dropped from the
subject. If two routes still reduce to the same name (e.g. `POST /users` and
`POST /users/{id}` both yield `CreateUsers`), the colliding one folds its path
params into the name — `CreateUsersById` — so RPC and message names stay unique.

The gRPC Connect bridge derives its URLs with the same algorithm, so a generated
Connect client and the served route agree.

## Which routes become RPCs

Every route the api plugin binds to a transport becomes one RPC. Routes finalized
with `api.Endpoint(...).Document()` are **skipped**: their handler is mounted
directly on the server, outside the api builder, so there is nothing for the
Connect bridge to invoke and an emitted RPC would advertise a URL that cannot
answer.

Those routes are still served, and they still appear in the OpenAPI document and
in generated typed clients — the asymmetry is deliberate, and it is the one place
where the two published contracts differ.

## Type Mapping

| Go | Proto |
|----|-------|
| `string` | `string` |
| `bool` | `bool` |
| `int` / `int32` | `int32` |
| `int64` | `int64` |
| `uint` / `uint32` | `uint32` |
| `uint64` | `uint64` |
| `float32` | `float` |
| `float64` | `double` |
| `[]T` | `repeated <T>` |
| `time.Time` | `google.protobuf.Timestamp` (import added automatically) |
| `time.Duration` | `google.protobuf.Duration` (import added automatically) |
| nested `struct` | `message <Name>` (registered once, by struct name) |
| `struct` with no exported fields | `bytes` |
| `map`, other kinds | `bytes` |

Emitted identifiers are sanitized to the proto grammar, and multi-line endpoint
descriptions emit one `//` comment per line, so the output stays protoc-valid
even for awkward Go input.

## Build-Time Generation

The Putnami Go extension's `describe` phase compiles a host binary, runs it with
`PUTNAMI_DESCRIBE=all`, and copies `schema/api.proto` into the project tree — so
you never have to start the app to refresh the committed proto. Set
`options.generate.schema=false` in `putnami.json` to skip the copy.

`Configure` writes nothing — at runtime or under describe mode. Rendering happens
in memory, so a served workload never writes into its working directory and never
fails to start because that directory is read-only.

To export the document outside a Putnami build — a `go run` step, a test, custom
tooling — ask for it explicitly:

```go
if err := protoPlugin.WriteTo(""); err != nil { // "" uses PluginOptions.Output
    return err
}
```

`WriteTo` is a no-op before `Configure` has produced a document.

> **Migrating.** Earlier versions wrote the file from `Configure` on every
> application start. Regenerate with `putnami build`, or call `WriteTo("")` after
> `Configure` if you drive the plugin outside Putnami.

## Generating Without a Plugin

`proto.Generate` is a pure function over discovered routes — handy for tests or
custom tooling that already has the route metadata:

```go
doc := proto.Generate(routes, proto.Options{PackageName: "myapp.v1"})
fmt.Println(doc.Content) // the rendered .proto source
```

`proto.Document` exposes the structured result too: `Content`, `PackageName`,
`Imports`, `Service`, and `Messages`.

## Best Practices

- Register the API plugin before the proto plugin so discovery completes first.
- Commit `schema/api.proto` and review its diff in PRs — deterministic output
  makes the diff a faithful record of your API surface changing.
- Give every endpoint a `Returns` type; a void response renders as an empty reply
  message rather than a typed one.
- Prefer named struct types over anonymous structs for reply/body shapes so the
  generated message names are stable and readable.

## Contract and compatibility

See the [API contracts specification](../../api/specs/api-contracts.json), the
[build-output ADR](adr/0001-contract-artifacts-are-build-outputs.md), and [support
evidence](../README.md#support-and-contract). The package is stable and
maintained, with the workspace's [pre-1.0 migration
policy](../../../../RELEASE.md), not strict compatibility between every `0.x`
minor.
