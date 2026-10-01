# Security

`go.putnami.dev/security` provides declarative authentication and authorization middleware for HTTP endpoints.

It is a **stable** public package owned by the Go SDD surface. Invalid,
incomplete, or unavailable authentication fails closed. Missing identity maps
to `401`; authenticated identity without required authority maps to `403`.
Its authentication and authorization specification and accepted fail-closed
decision record live next to the package source.

## Identity resolution

Before authorization, you need to populate `ctx.User` with the authenticated user's claims. Use `IdentityResolver` to extract the identity from the request:

```go
import (
    fhttp "go.putnami.dev/http"
    "go.putnami.dev/security"
)

server := fhttp.NewServerPlugin(fhttp.ServerConfig{Port: 3000})

server.Use(security.IdentityResolver(func(ctx *fhttp.Context) *fhttp.Claims {
    token := ctx.Header("Authorization")
    if token == "" {
        return nil // not authenticated
    }
    claims, err := verifyJWT(strings.TrimPrefix(token, "Bearer "))
    if err != nil {
        return nil
    }
    return fhttp.ClaimsFromMap(claims)
}))
```

The resolver sets `ctx.User` if authentication succeeds. Returning `nil` means the request is unauthenticated — downstream authorization middleware will return `401`.

## Declarative authorization

Use `security.Options` to define role, scope, and client-based access control:

```go
import "go.putnami.dev/security"

// Require specific roles
server.GET("/admin", adminHandler)
server.Use(security.Middleware(security.Options{
    Roles: []string{"admin"}, // ALL listed roles required
}))

// Require at least one role
security.Middleware(security.Options{
    RolesAny: []string{"admin", "moderator"}, // at least ONE required
})

// Require scopes
security.Middleware(security.Options{
    Scopes: []string{"read", "write"}, // ALL scopes required
})

// Require at least one scope
security.Middleware(security.Options{
    ScopesAny: []string{"read", "write"}, // at least ONE required
})

// Restrict to specific client IDs
security.Middleware(security.Options{
    Client: []string{"web-app", "mobile-app"},
})
```

### Combining rules

All rules in a single `Options` are AND'd together:

```go
security.Middleware(security.Options{
    Roles:     []string{"admin"},
    Scopes:    []string{"write"},
    Client:    []string{"internal-service"},
})
// Must have admin role AND write scope AND be from internal-service
```

### Options reference

| Field | Type | Logic | Description |
|-------|------|-------|-------------|
| `Roles` | `[]string` | AND | All listed roles must be present |
| `RolesAny` | `[]string` | OR | At least one listed role must be present |
| `Scopes` | `[]string` | AND | All listed scopes must be present |
| `ScopesAny` | `[]string` | OR | At least one listed scope must be present |
| `Client` | `[]string` | OR | Client ID must match one of the listed values |

## Custom guards

For authorization logic that goes beyond declarative rules, use a guard function:

```go
// Only allow users to access their own resources
server.Use(security.Middleware(security.Guard(
    func(user *fhttp.Claims, ctx *fhttp.Context) bool {
        return user.Subject == ctx.Param("userId")
    },
)))
```

Guards receive the authenticated user claims and the request context. Return `true` to allow, `false` to deny with `403`.

## Per-route authorization

Apply authorization middleware to specific routes:

```go
server := fhttp.NewServerPlugin(fhttp.ServerConfig{Port: 3000})

// Global: identity resolution
server.Use(security.IdentityResolver(resolveUser))

// Public routes
server.GET("/health", healthHandler)
server.GET("/login", loginHandler)

// Protected routes use Chain
adminOnly := fhttp.Chain(
    security.Middleware(security.Options{Roles: []string{"admin"}}),
)

server.GET("/admin/users", adminOnly(listUsersHandler))
server.DELETE("/admin/users/{id}", adminOnly(deleteUserHandler))
```

## Endpoint-level security

The API endpoint builder accepts the same security rules and records their
roles and scopes in generated OpenAPI metadata:

```go
api.Endpoint("DELETE", "/users/{id}").
    Secure(security.Options{
        Roles:  []string{"admin"},
        Scopes: []string{"admin:write"},
    }).
    Handle(deleteUser)
```

## Claim formats

The security middleware handles multiple claim formats:

### Roles

Extracted from `claims["roles"]`:

```go
// Slice of strings
{"roles": []string{"admin", "user"}}

// Slice of any
{"roles": []any{"admin", "user"}}

// Space-separated string
{"roles": "admin user moderator"}
```

### Scopes

Extracted from `claims["scope"]` (OAuth2 convention):

```go
// Slice of strings
{"scope": []string{"read", "write"}}

// Space-separated string (OAuth2 standard)
{"scope": "read write delete"}
```

### Client ID

Extracted from `claims["client_id"]`:

```go
{"client_id": "web-app"}
```

## HTTP responses

| Status | When |
|--------|------|
| `401 Unauthorized` | `ctx.User` is nil (no identity resolved) |
| `403 Forbidden` | Authorization check failed |

## Related guides

- [HTTP & Middleware](/docs/frameworks/go/http) — middleware registration
- [OpenAPI](/docs/frameworks/go/openapi) — generated endpoint security metadata
