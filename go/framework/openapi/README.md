# OpenAPI

The `openapi` package generates OpenAPI 3.0.3 specifications from registered HTTP endpoints. It introspects Go struct types to produce parameter, request body, and response schemas.

## Quick Start

```go
import (
    "go.putnami.dev/errors"
    "go.putnami.dev/openapi"
)

plugin := openapi.NewPlugin(openapi.PluginOptions{
    Title:       "My API",
    Version:     "1.0.0",
    Description: "REST API for my service",
    Servers: []openapi.Server{
        {URL: "https://api.example.com"},
    },
})

// Register routes for spec generation
plugin.AddRoute(openapi.DiscoveredRoute{
    Method:      "GET",
    Path:        "/users",
    Description: "List all users",
    Query:       reflect.TypeOf(ListUsersQuery{}),
    Returns:     reflect.TypeOf([]User{}),
})

plugin.AddRoute(openapi.DiscoveredRoute{
    Method:      "POST",
    Path:        "/users",
    Description: "Create a new user",
    Body:        reflect.TypeOf(CreateUserInput{}),
    Returns:     reflect.TypeOf(User{}),
    ReturnsStatus: 201,
    ReturnsDescription: "Created",
    ErrorCodes:  []errors.Code{errors.CodeConflict},
    Throws: []openapi.ThrowsMeta{
        {Status: 400, Description: "Validation failed"},
    },
    Security: &openapi.SecurityMeta{Roles: []string{"admin"}},
})

// Register on HTTP server to serve at /_/openapi.json
plugin.RegisterOn(httpServer)

// Add to application
a.Module.Use(plugin)
```

## Route Discovery

Each `DiscoveredRoute` captures endpoint metadata:

| Field | Type | Description |
|-------|------|-------------|
| `Method` | `string` | HTTP method (GET, POST, PUT, DELETE, PATCH) |
| `Path` | `string` | Route path with `{param}` or `[param]` syntax |
| `Description` | `string` | Human-readable operation description |
| `Tags` | `[]string` | Grouping tags for the operation |
| `Params` | `reflect.Type` | Struct type for path parameter validation |
| `Query` | `reflect.Type` | Struct type for query parameter validation |
| `Body` | `reflect.Type` | Struct type for request body validation |
| `Returns` | `reflect.Type` | Struct type for the success response |
| `ReturnsStatus` | `int` | Primary 2xx status; zero defaults to 200 |
| `ReturnsDescription` | `string` | Primary success description |
| `ErrorCodes` | `[]errors.Code` | Framework-known errors mapped to standard error responses |
| `Throws` | `[]ThrowsMeta` | Error responses (status, description, schema) |
| `Security` | `*SecurityMeta` | Required roles and scopes |

When using `From(apiPlugin)`, the plugin refreshes auto-discovered routes after
all app configurers have completed for both serving and build-time describe
output. This keeps the spec stable when route-owning plugins share one
`api.Plugin` and are registered around the OpenAPI plugin.

## Schema Generation

Schemas are generated from Go struct types using reflection and struct tags:

```go
type CreateUserInput struct {
    Name  string `json:"name" validate:"required,minlen=2"`
    Email string `json:"email" validate:"required,email"`
    Age   int    `json:"age" validate:"min=0,max=150"`
}
```

Field names, anonymous-field promotion, and collision dominance match
`encoding/json`. An absent or invalid explicit JSON tag name falls back to the
exact Go field name. A `default` tag is emitted as a typed OpenAPI `default`;
an invalid value makes document serialization fail instead of publishing a
different contract.

Produces:

```json
{
  "type": "object",
  "required": ["email", "name"],
  "properties": {
    "name": { "type": "string" },
    "email": { "type": "string", "format": "email" },
    "age": { "type": "integer", "format": "int64" }
  }
}
```

### Type Mapping

| Go Type | OpenAPI Type | Format |
|---------|-------------|--------|
| `string` | `string` | — |
| `int`, `int64` | `integer` | `int64` |
| `float32` | `number` | `float` |
| `float64` | `number` | `double` |
| `bool` | `boolean` | — |
| `[]T` | `array` | — |
| `struct` | `object` | — |

### Validate Tag Hints

| Tag | OpenAPI Effect |
|-----|----------------|
| `required` | Added to `required` array |
| `uuid` | `format: "uuid"` |
| `email` | `format: "email"` |
| `default` | Typed `default`; invalid values fail document serialization |

### Field Descriptions

Use the `description` struct tag for field-level documentation:

```go
type UserParams struct {
    ID string `json:"id" validate:"required,uuid" description:"User ID"`
}
```

## First-party client contract

`api.WithClientService(...)` turns the document into a strict first-party client
contract: `x-putnami-client` on the document and on every operation. The plugin
publishes only what this provider can honor, and fails generation with the route
named otherwise.

| Rule | Effect |
| --- | --- |
| Server stream | Published over SSE only |
| Client / bidirectional stream | Refused — no first-party WebSocket server yet |
| Integer schema | Carries `format` and exact `minimum` / `maximum` |
| `GET` / `HEAD` request body | Refused |
| `Client(api.ClientOperationOptions{External: "…"})` | The operation carries `x-putnami-external-contract` instead of `x-putnami-client` |

An operation an external authority owns stays in the document with its
parameters, body and responses, documented the way a provider without a contract
documents them. Every first-party reader skips it, so no generated client and no
protobuf descriptor includes it. Component schemas are shared by the whole
document and stay first-party.

## Security

When a route has `Security` metadata, the spec includes:
- A `bearerAuth` security scheme in `components`
- `security` requirement on the operation
- Roles and scopes documented in the description

```go
Security: &openapi.SecurityMeta{
    Roles:  []string{"admin"},
    Scopes: []string{"users:write"},
}
```

## Programmatic Access

Generate specs without the plugin for custom use cases:

```go
doc := openapi.GenerateSpec(routes, openapi.Options{
    Title:   "My API",
    Version: "1.0.0",
})

// Serialize to canonical JSON
data, err := doc.JSON()
```

## HTTP Endpoint

The plugin serves the spec at `/_/openapi.json` by default (configurable via `PluginOptions.Route`).

`RegisterOn` registers an **unauthenticated** GET handler, and the spec discloses the API's security model (required roles/scopes and internal paths). The response is sent with `Cache-Control: no-store` by default so shared caches/CDNs do not retain it. If you intend the spec to be public and cacheable, set `PluginOptions.CacheControl` explicitly (e.g. `"public, max-age=3600"`); if it should be private, place the route behind auth middleware and keep the default `no-store`.

## Support and contract

The SDD owner is `go`. `go.putnami.dev/openapi` is public, documented, maintained,
and classified `stable` in the workspace [support
catalog](../../../putnami.support.json). Before v1.0.0, a minor `0.x` release may
still contain a breaking change; the [release policy](../../../RELEASE.md) requires
release notes and migration documentation rather than strict compatibility between
every pre-1.0 minor.

This package does not own a feature of its own: it renders one half of the
[`go/api-contracts`](../api/putnami.features.json) outcome, whose
[specification](../api/specs/api-contracts.json) lives with
[`go.putnami.dev/api`](../api). Its own durable decision is [publish the final
route set, and do not let caches keep
it](doc/adr/0001-publish-the-final-route-set.md).

Regression evidence covers [spec generation, schema mapping, and security
schemes](openapi_test.go), [auto-discovery and late route
refresh](api_integration_test.go), [describe output](describe_test.go), and [the
served route and its cache directive](register_on_test.go).
