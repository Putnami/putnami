# OpenAPI

`go.putnami.dev/openapi` generates OpenAPI 3.0.3 specifications from Go struct types and endpoint metadata.

## Plugin setup

```go
import (
    "go.putnami.dev/app"
    fhttp "go.putnami.dev/http"
    "go.putnami.dev/openapi"
)

server := fhttp.NewServerPlugin(fhttp.ServerConfig{Port: 3000})
oapiPlugin := openapi.NewPlugin(openapi.PluginOptions{})

// Register the plugin — serves the spec at /_/openapi.json
oapiPlugin.RegisterOn(server)

a := app.New("my-service")
a.Module.Use(server)
a.Module.Use(oapiPlugin)
```

The spec is served at `GET /_/openapi.json`.

## Documenting endpoints

Register endpoints with the plugin using the endpoint builder:

```go
import "reflect"

type CreateUserBody struct {
    Name  string `json:"name" validate:"required,minlen=2" description:"User's display name"`
    Email string `json:"email" validate:"required,email" description:"Email address"`
}

type UserResponse struct {
    ID    string `json:"id" description:"User ID"`
    Name  string `json:"name" description:"User's display name"`
    Email string `json:"email" description:"Email address"`
}

type UserParams struct {
    ID string `json:"id" validate:"required,uuid" description:"User ID"`
}

// Register endpoint with OpenAPI metadata
oapiPlugin.AddRoute(openapi.DiscoveredRoute{
    Method:      "POST",
    Path:        "/users",
    Description: "Create a new user",
    Tags:        []string{"Users"},
    Body:        reflect.TypeOf(CreateUserBody{}),
    Returns:     reflect.TypeOf(UserResponse{}),
    Throws: []openapi.ThrowsMeta{
        {Status: 409, Description: "Email already exists"},
        {Status: 422, Description: "Validation failed"},
    },
})

oapiPlugin.AddRoute(openapi.DiscoveredRoute{
    Method:      "GET",
    Path:        "/users/{id}",
    Description: "Get a user by ID",
    Tags:        []string{"Users"},
    Params:      reflect.TypeOf(UserParams{}),
    Returns:     reflect.TypeOf(UserResponse{}),
    Throws: []openapi.ThrowsMeta{
        {Status: 404, Description: "User not found"},
    },
})
```

### DiscoveredRoute fields

| Field | Type | Description |
|-------|------|-------------|
| `Method` | `string` | HTTP method |
| `Path` | `string` | Route path (supports `{param}` and `[param]`) |
| `Description` | `string` | Operation description |
| `Tags` | `[]string` | Grouping tags |
| `Params` | `reflect.Type` | Path parameters struct type |
| `Query` | `reflect.Type` | Query parameters struct type |
| `Body` | `reflect.Type` | Request body struct type |
| `Returns` | `reflect.Type` | Success response struct type |
| `Throws` | `[]ThrowsMeta` | Possible error responses |
| `Security` | `*SecurityMeta` | Security requirements |

## Schema generation

The plugin generates OpenAPI schemas by introspecting Go struct types using reflection and struct tags.

### Type mapping

| Go type | OpenAPI type | Format |
|---------|-------------|--------|
| `string` | `string` | — |
| `int`, `int64` | `integer` | `int64` |
| `float32` | `number` | `float` |
| `float64` | `number` | `double` |
| `bool` | `boolean` | — |
| `[]T` | `array` | — |
| `struct` | `object` | — |

### Struct tags for OpenAPI

| Tag | OpenAPI mapping | Example |
|-----|-----------------|---------|
| `json:"name"` | Property name | `json:"email"` |
| `description:"text"` | Field description | `description:"User email"` |
| `validate:"required"` | Added to `required` array | `validate:"required"` |
| `validate:"uuid"` | `format: "uuid"` | `validate:"uuid"` |
| `validate:"email"` | `format: "email"` | `validate:"email"` |

### Example schema

```go
type Product struct {
    ID          string   `json:"id" validate:"required,uuid" description:"Product ID"`
    Name        string   `json:"name" validate:"required" description:"Product name"`
    Price       float64  `json:"price" validate:"required,min=0" description:"Price in USD"`
    Tags        []string `json:"tags" description:"Product tags"`
    Description string   `json:"description" description:"Product description"`
}
```

Generates:

```json
{
  "type": "object",
  "required": ["id", "name", "price"],
  "properties": {
    "id": {
      "type": "string",
      "format": "uuid",
      "description": "Product ID"
    },
    "name": {
      "type": "string",
      "description": "Product name"
    },
    "price": {
      "type": "number",
      "format": "double",
      "minimum": 0,
      "description": "Price in USD"
    },
    "tags": {
      "type": "array",
      "items": { "type": "string" },
      "description": "Product tags"
    },
    "description": {
      "type": "string",
      "description": "Product description"
    }
  }
}
```

## Programmatic access

Generate the spec without the plugin:

```go
doc := openapi.GenerateSpec(routes, openapi.Options{
    Title:   "My API",
    Version: "1.0.0",
})

specJSON, err := doc.JSON()
```

## Security metadata

Document security requirements:

```go
oapiPlugin.AddRoute(openapi.DiscoveredRoute{
    Method:      "DELETE",
    Path:        "/users/{id}",
    Description: "Delete a user",
    Security: &openapi.SecurityMeta{
        Roles:  []string{"admin"},
        Scopes: []string{"write"},
    },
})
```

## Route discovery is refreshed, not snapshotted

Plugins configure in registration order, so a plugin registered after the OpenAPI
plugin can still add routes to the shared `api.Plugin`. The document is therefore
re-rendered at every point where it is observed — configure, start, describe, and
the first runtime request — rather than snapshotted once. Ordering the OpenAPI
plugin before or after a route-registering plugin produces the same document.

## Serving the document safely

The document enumerates internal paths and states which roles and scopes each
operation requires, and `RegisterOn` mounts it as an **unauthenticated** GET. It
is therefore served with `Cache-Control: no-store` by default, so a shared cache
or CDN cannot retain it and keep serving it after the route is placed behind
auth.

```go
// Public and cacheable — an explicit decision:
openapi.NewPlugin(openapi.PluginOptions{CacheControl: "public, max-age=3600"})

// Private — put the route behind auth middleware and keep the default.
```

## Committing the document

`putnami build` writes `schema/openapi.json` (plus a gzipped companion under
`.gen/`). The client generator reads the document in memory from the plugin, not
from disk, so client generation never races the describe output.

## Related guides

- [HTTP & Middleware](/docs/frameworks/go/http) — endpoint registration
- [Validation](/docs/frameworks/go/validation) — validation tags
- [Security](/docs/frameworks/go/security) — authorization
## Support and contract

`go.putnami.dev/openapi` is `stable` in the workspace support catalog. It renders
one half of the API contracts specification owned by `go.putnami.dev/api`
(`go/framework/api/specs/`), with its own accepted decision record under
`go/framework/openapi/doc/adr/`. Before v1.0.0 a minor `0.x` release may still
contain a documented breaking change; strict compatibility between every pre-1.0
minor is not promised.
