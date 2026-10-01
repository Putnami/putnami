# Security

The `security` package provides declarative authorization middleware for HTTP endpoints.

## Options-Based Authorization

Use `Options` for declarative role/scope/client checks:

```go
// Require all listed roles
security.Middleware(security.Options{
    Roles: []string{"admin", "editor"},
})

// Require at least one role
security.Middleware(security.Options{
    RolesAny: []string{"admin", "editor"},
})

// Require all scopes
security.Middleware(security.Options{
    Scopes: []string{"read", "write"},
})

// Require at least one scope
security.Middleware(security.Options{
    ScopesAny: []string{"admin", "write"},
})

// Restrict to specific client IDs
security.Middleware(security.Options{
    Client: []string{"my-app", "admin-ui"},
})
```

Returns **401** if no user is set on the context, **403** if authorization fails.

## User Claims

`ctx.User` is a typed `*phttp.Claims` struct:

```go
type Claims struct {
    Subject  string         // JWT "sub"
    Issuer   string         // JWT "iss"
    ClientID string         // OAuth2 client ID
    Roles    []string       // user roles
    Scopes   []string       // OAuth2 scopes
    Extra    map[string]any // custom claims
}
```

Use `phttp.ClaimsFromMap(m)` to convert a `map[string]any` (e.g., parsed JWT) into typed claims.

## Custom Guards

For custom authorization logic, use a `Guard` function:

```go
security.Middleware(security.Guard(func(user *phttp.Claims, ctx *phttp.Context) bool {
    // Check if the user owns the resource
    return user.Subject == ctx.Param("userId")
}))
```

## Identity Resolution

`IdentityResolver` is middleware that populates `ctx.User` from request data (e.g., a JWT):

```go
resolver := security.IdentityResolver(func(ctx *phttp.Context) *phttp.Claims {
    token := ctx.Header("Authorization")
    if token == "" {
        return nil
    }
    rawClaims, err := validateJWT(token)
    if err != nil {
        return nil
    }
    return phttp.ClaimsFromMap(rawClaims)
})

server.Use(resolver)
```

The resolver runs before authorization middleware, so `ctx.User` is available for role/scope checks.

## Observability

Every authorization decision is logged and counted so denials are never silent.

- **Structured logs** (via the framework logger, named `security`): grants log at
  `Debug`, denials at `Warn`. Each entry carries `decision`, `status` (200/401/403),
  `subject` (`anonymous` when unauthenticated), `method`, `path`, the failing
  `reason` (e.g. `missing required role`), and `requestId` when the `RequestID`
  middleware is installed — so an outcome can be correlated with the access log.
  The token, secret, and the exact missing scope/role are never logged or returned
  to the client.
- **Counters** (stdlib `expvar`, published as `security.auth_decisions`): a map of
  decision label → count (`allow`, `deny_unauthenticated`, `deny_client`,
  `deny_scope`, `deny_role`, `deny_guard`). Expose them via `expvar.Handler()` or
  `/debug/vars` to alert on 401/403 spikes (credential stuffing, brute force).

## Endpoint-Level Security

The API endpoint builder accepts the same rules and records declared roles and
scopes in generated OpenAPI metadata:

```go
api.Endpoint("DELETE", "/users/{id}").
    Secure(security.Options{Roles: []string{"admin"}}).
    Handle(deleteUser)
```

## Support and contract

The SDD owner is `go`. `go.putnami.dev/security` is public, documented,
maintained, and classified **stable** in the repository
[support catalog](../../../putnami.support.json). Before v1.0, minor releases
may still require documented migrations; see the repository
[release policy](../../../RELEASE.md).

The durable authentication and authorization contract is
[`go/authentication-authorization`](specs/authentication-authorization.json).
Fail-closed decisions and non-disclosure are recorded in
[ADR 0001](doc/adr/0001-fail-closed-authorization-decisions.md) and protected by
[`security_test.go`](security_test.go), [`jwt_test.go`](jwt_test.go), and
[`secret_test.go`](secret_test.go).
