# OpenAPI Specification Generator

`go.putnami.dev/openapi` generates OpenAPI 3.0.3 specifications from registered HTTP endpoints. It introspects endpoint metadata -- schemas, parameters, responses, and security requirements -- to produce a complete API document and optionally serve it over HTTP.

## Overview

The module provides three main capabilities:

1. **Auto-discovery (recommended)** -- `Plugin.From(*api.Plugin)` derives the spec directly from typed `api.Endpoint` definitions, so the documentation cannot drift from the real API.
2. **Spec generation** -- the `GenerateSpec` function converts a list of `DiscoveredRoute` descriptors into a fully typed `Document` (OpenAPI 3.0.3).
3. **Plugin integration** -- `Plugin` implements `app.Plugin` so the spec is generated during the configure phase and served as JSON at a configurable HTTP endpoint.

## Quick Start

The recommended integration is auto-discovery: define your routes once as typed
`api.Endpoint` values on an `api.Plugin` (see `go.putnami.dev/api`), then wire the
OpenAPI plugin to it with `From`. The spec — request/response schemas, params,
and error responses — is derived from those definitions, so it can never drift
from the real API.

```go
package main

import (
    "go.putnami.dev/api"
    "go.putnami.dev/app"
    "go.putnami.dev/errors"
    "go.putnami.dev/http"
    "go.putnami.dev/openapi"
)

func main() {
    server := http.NewServerPlugin(http.ServerConfig{Port: 8080})

    // Define routes as typed api.Endpoint values on the api plugin.
    apiPlugin := api.New(server)

    oa := openapi.NewPlugin(openapi.PluginOptions{
        Title:       "My Service API",
        Version:     "1.0.0",
        Description: "User management service",
        Servers: []openapi.Server{
            {URL: "https://api.example.com", Description: "Production"},
        },
    }).From(apiPlugin) // auto-discover routes from the api plugin

    a := app.New("my-service")
    a.Use(server)
    a.Use(apiPlugin)
    a.Use(oa) // serves GET /_/openapi.json
    if err := a.ListenAndServe(); err != nil {
        panic(err)
    }
}
```

After startup, the spec is available at `GET /_/openapi.json`. If you build routes
without the `api` plugin, you can still feed the spec manually with `AddRoute` —
see [Registering Routes](#registering-routes) for that lower-level path.

## Plugin Options

`PluginOptions` configures the plugin:

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `Title` | `string` | `"API"` | API title in the spec info block |
| `Version` | `string` | `"1.0.0"` | API version in the spec info block |
| `Description` | `string` | `""` | Human-readable API description |
| `Servers` | `[]Server` | `nil` | List of target servers (URL + description) |
| `Route` | `string` | `"/_/openapi.json"` | HTTP path where the spec is served |
| `CacheControl` | `string` | `"no-store"` | Cache-Control header for the served spec |

## Registering Routes

`From(apiPlugin)` (see [Quick Start](#quick-start)) is the recommended way to
populate the spec and keeps it in lock-step with the real API. `AddRoute` is the
lower-level escape hatch for callers that construct routes without the `api`
plugin. Routes are described with `DiscoveredRoute`, and each field maps directly
to an OpenAPI operation:

```go
oa.AddRoute(openapi.DiscoveredRoute{
    Method:      "POST",
    Path:        "/users",
    Description: "Create a new user",
    Tags:        []string{"users"},
    Body:        reflect.TypeOf(CreateUserBody{}),
    Returns:     reflect.TypeOf(UserResponse{}),
    ErrorCodes:  []errors.Code{errors.CodeConflict},
    Throws: []openapi.ThrowsMeta{
        {Status: 400, Description: "Validation error", Schema: reflect.TypeOf(ErrorResponse{})},
        {Status: 409, Description: "User already exists"},
    },
    Security: &openapi.SecurityMeta{
        Roles:  []string{"admin"},
        Scopes: []string{"users:write"},
    },
})
```

### DiscoveredRoute Fields

| Field | Type | Purpose |
|-------|------|---------|
| `Method` | `string` | HTTP method (`GET`, `POST`, `PUT`, `PATCH`, `DELETE`) |
| `Path` | `string` | Route path; supports `{param}` and `[param]` syntax |
| `Description` | `string` | Operation description |
| `Tags` | `[]string` | Grouping tags for the operation |
| `Params` | `reflect.Type` | Struct type describing path parameters |
| `Query` | `reflect.Type` | Struct type describing query parameters |
| `Body` | `reflect.Type` | Struct type describing the request body (used for POST, PUT, PATCH) |
| `Returns` | `reflect.Type` | Struct type describing the primary success response body |
| `ReturnsStatus` | `int` | Primary 2xx status; zero defaults to 200 |
| `ReturnsDescription` | `string` | Primary success description; empty uses the default |
| `ErrorCodes` | `[]errors.Code` | Framework-known errors mapped through `errors.HTTPStatusForCode` |
| `ErrorDetails` | `map[errors.Code]reflect.Type` | The type of the `details` member each declared code carries |
| `Throws` | `[]ThrowsMeta` | Error responses with status codes, descriptions, and optional schemas |
| `Security` | `*SecurityMeta` | Roles and scopes required for the endpoint |

## Schema Generation from Structs

The generator derives OpenAPI schemas from Go struct types using reflection. It reads `json`, `validate`, and `description` struct tags to produce accurate property definitions.

### Struct Tag Mapping

```go
type CreateUserBody struct {
    Name  string `json:"name"  validate:"required,minlen=2" description:"User's display name"`
    Email string `json:"email" validate:"required,email"`
    Age   int    `json:"age"   validate:"min=0,max=150"`
}
```

| Tag | Effect |
|-----|--------|
| `json:"name"` | Sets the property name. An absent or invalid name falls back to the exact Go field name. Anonymous promotion and collision dominance match `encoding/json`. |
| `validate:"required"` | Adds the field to the schema's `required` array |
| `validate:"email"` | Sets `format: "email"` on the property |
| `validate:"uuid"` | Sets `format: "uuid"` on the property |
| `validate:"min=N"` / `validate:"max=N"` | Sets `minimum` / `maximum` |
| `validate:"minlen=N"` / `validate:"maxlen=N"` | Sets `minLength` / `maxLength` |
| `validate:"pattern=RE"` | Sets `pattern` |
| `validate:"oneof=a\|b\|c"` | Sets `enum` to `[a, b, c]` |
| `default:"..."` | Sets a typed `default`; invalid values make document serialization fail |
| `description:"..."` | Sets the `description` field on the property |

These constraints apply to body, response, path-parameter, and query-parameter schemas alike.

### Go Type to OpenAPI Type Mapping

| Go type | OpenAPI type | Format |
|---------|-------------|--------|
| `string` | `string` | -- |
| `int`, `int64`, `uint` | `integer` | `int64` |
| `float32` | `number` | `float` |
| `float64` | `number` | `double` |
| `bool` | `boolean` | -- |
| `time.Time` | `string` | `date-time` |
| `time.Duration` | `integer` | `int64` |
| `[]byte`, `json.RawMessage` | `string` / `object` | `byte` / -- |
| `[]T` | `array` | items derived from `T` |
| `struct` | `object` | properties derived from fields |
| `map` | `object` | -- |

`time.Time` and `[]byte` are special-cased because they marshal as an RFC 3339 string and a base64 string respectively — mapping them purely by reflected kind would document them as an empty object and an array of integers.

### Nested and Recursive Structs

Nested struct fields are expanded inline as nested `object` schemas with their own properties. Self-referential types (e.g. a category whose `children` are categories) are emitted once under `components.schemas` and referenced with `$ref`, which both breaks the cycle and deduplicates the schema:

```go
type Address struct {
    Street string `json:"street"`
    City   string `json:"city"`
}

type UserResponse struct {
    ID      string  `json:"id" validate:"uuid"`
    Name    string  `json:"name"`
    Address Address `json:"address"`
}
```

The `address` property is generated as a nested object with `street` and `city` string properties.

## Path Parameters

Path parameters are extracted automatically from `{param}` or `[param]` segments in the route path. When a `Params` struct type is provided, the generator uses its field types and tags for richer parameter definitions:

```go
type UserParams struct {
    ID string `json:"id" validate:"required,uuid" description:"User ID"`
}

oa.AddRoute(openapi.DiscoveredRoute{
    Method:  "GET",
    Path:    "/users/{id}",
    Params:  reflect.TypeOf(UserParams{}),
    Returns: reflect.TypeOf(UserResponse{}),
})
```

This produces a path parameter with `type: string`, `format: uuid`, and a description. Path parameters are always marked as `required: true`.

Both `{id}` and `[id]` path syntax are supported. The `[param]` form is converted to `{param}` in the generated spec.

## Query Parameters

Query parameters are derived from a struct type passed as `Query`. Each exported field becomes a query parameter:

```go
type ListUsersQuery struct {
    Page  int    `json:"page"  validate:"min=1"`
    Limit int    `json:"limit" validate:"min=1,max=100"`
    Sort  string `json:"sort"  description:"Sort field"`
}

oa.AddRoute(openapi.DiscoveredRoute{
    Method:  "GET",
    Path:    "/users",
    Query:   reflect.TypeOf(ListUsersQuery{}),
    Returns: reflect.TypeOf([]UserResponse{}),
})
```

Fields with `validate:"required"` are marked as required query parameters.

## Error Responses

Every operation includes framework default `400` and `500` JSON error responses.
Declare framework-known route errors with `ErrorCodes`; each code is mapped through
`errors.HTTPStatusForCode` and rendered with the standard error envelope:

```go
ErrorCodes: []errors.Code{errors.CodeNotFound, errors.CodeConflict},
```

`ErrorDetails` (`api.Endpoint(...).MayThrowDetails(code, api.Type[T]())`) names
the type of a declared code's `details` member. It becomes that error's
`x-putnami-client` schema, which describes `details` and never the envelope
(ADR 0006), and the error response at that status documents `details` as `T`.
When several codes share the status, it documents `anyOf` their types, in the
byte order of the codes and each type once. `anyOf` admits a body that more than
one type describes, which `oneOf` would refuse, and the envelope's `code` tells
the errors apart. The TypeScript provider uses the same combinator and order. A
code with no entry publishes no schema. A `Throws` schema is not a details
declaration: it keeps documenting the whole response body.

For endpoints declared through `api.Endpoint`, `Returns(type)` produces the
compatible primary `200` response. Use
`ReturnsStatus(status, description, type)` for another primary 2xx response on a
unary endpoint, such as `201 Created`; the plugin does not add a synthetic 200.
Generated clients retain its response type. `Response(...)` and
`AdditionalReturns` add other success statuses.

Use `ThrowsMeta` for custom descriptions or schemas. Each entry maps to a non-200
response in the operation and overrides generated responses for the same status:

```go
Throws: []openapi.ThrowsMeta{
    {Status: 404, Description: "User not found"},
    {Status: 400, Description: "Bad request", Schema: reflect.TypeOf(ErrorResponse{})},
}
```

When `Schema` is provided, the response includes an `application/json` content definition with the generated schema. When omitted, only the description is included.

In a first-party client contract, each `Throws` status needs at least one declared error code at that status: a `MayThrow` code, or the implicit `http.bad_request` (400) and `http.internal_server` (500) codes every first-party endpoint declares. Several codes can share a status, such as `errors.CodeConflict` and `errors.CodeAlreadyExists` on `409`: the codes tell a generated client which error it received, and `Throws` documents the status. A `Throws` status that no declared code shares has no stable wire code, and the plugin refuses the contract. The TypeScript `.throws()` applies the same rule.

A `Throws` schema documents the response in `responses` only. It never becomes a declared error's `details` schema in the client contract, so the implicit 400 and 500 codes, and every `MayThrow` code, carry none. `MayThrowDetails` is the only declaration of a `details` schema, and a `Throws` at the same status leaves it in place.

## Security

Setting `Security` on a route adds a `bearerAuth` security requirement to the operation and appends role/scope information to the operation description:

```go
Security: &openapi.SecurityMeta{
    Roles:  []string{"admin"},
    Scopes: []string{"users:delete"},
},
```

When any route has security metadata, the generated spec includes a `components.securitySchemes` section with a `bearerAuth` scheme (type `http`, scheme `bearer`, format `JWT`).

The operation description is automatically extended with the required roles and scopes (for example, "Required roles: admin. Required scopes: users:delete.").

## Standalone Spec Generation

You can generate a spec without the plugin, using `GenerateSpec` directly:

```go
doc := openapi.GenerateSpec(routes, openapi.Options{
    Title:       "My API",
    Version:     "2.0.0",
    Description: "Generated from route metadata",
    Servers: []openapi.Server{
        {URL: "https://api.example.com"},
    },
})

data, err := doc.JSON()
if err != nil {
    log.Fatal(err)
}
fmt.Println(string(data))
```

`Document.JSON()` returns canonical indented JSON bytes with stable object keys
and a trailing newline. The plugin uses those exact bytes for its runtime and
describe surfaces.

## Operation IDs

Operation IDs are generated automatically from the HTTP method and path. The method is lowercased, path segments are capitalized, and parameter braces are stripped:

| Method | Path | Operation ID |
|--------|------|-------------|
| `GET` | `/users` | `getUsers` |
| `POST` | `/users` | `postUsers` |
| `GET` | `/users/{id}` | `getUsers_Id` |
| `DELETE` | `/users/{id}/posts/{postId}` | `deleteUsers_Id_Posts_PostId` |
| `GET` | `/` | `get` |

## Serving the Spec

When wired with `RegisterOn`, the plugin registers an **unauthenticated** GET handler on the configured route (default `/_/openapi.json`). Because the document discloses the API's security model (required roles/scopes, internal paths), the response is sent with `Cache-Control: no-store` by default so shared caches/CDNs do not retain it. Override via `PluginOptions.CacheControl` (e.g. `"public, max-age=3600"`) when the spec is meant to be public, and gate the route behind auth middleware when it is not.

If the spec has not yet been generated (configure has not run), the endpoint returns a `503 Service Unavailable` response.

## Plugin Lifecycle

The plugin follows the standard `app.Plugin` lifecycle:

| Phase | Behavior |
|-------|----------|
| **Configure** | Warms the spec from currently registered routes |
| **Start** | Refreshes the served spec after all configurers have completed |
| **Describe** | Refreshes and writes the build-time spec from the final route set |
| **Stop** | No-op |

Call `AddRoute` before the configure phase runs. When using `From(apiPlugin)`, route-owning configurers may share the same API plugin; the OpenAPI plugin refreshes after the full configure phase before serving or writing describe artifacts.

## Best Practices

- **Prefer `From(apiPlugin)` auto-discovery.** Deriving the spec from typed `api.Endpoint` definitions keeps the documentation in lock-step with the real API. If you use manual `AddRoute`, keep the call next to the corresponding route registration to avoid drift between the actual API and the documented spec.
- **Use typed structs for all inputs and outputs.** Providing `Params`, `Query`, `Body`, and `Returns` struct types produces the most complete and accurate spec. Omitting them results in operations with minimal schema information.
- **Add `validate` tags to struct fields.** The `required`, `email`, `uuid`, `min`/`max`, `minlen`/`maxlen`, `pattern`, and `oneof` constraints are reflected in the generated schema, improving spec accuracy.
- **Add `description` tags for human-readable documentation.** Parameter and property descriptions make the generated spec more useful for API consumers.
- **Declare framework errors with `MayThrow` / `ErrorCodes`.** Use `MayThrowDetails` when an error carries a typed `details` body, and `Throws` only when a status needs a custom description or schema.
- **Use `Tags` to group related operations.** Tags organize endpoints in generated documentation UIs like Swagger UI.

## Contract and compatibility

See the [API contracts specification](../../api/specs/api-contracts.json), the
[final-route-set ADR](adr/0001-publish-the-final-route-set.md), and [support
evidence](../README.md#support-and-contract). The package is stable and
maintained, with the workspace's [pre-1.0 migration
policy](../../../../RELEASE.md), not strict compatibility between every `0.x`
minor.
