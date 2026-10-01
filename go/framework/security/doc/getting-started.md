# Security

The `go.putnami.dev/security` package provides authentication and authorization middleware for the Putnami Go HTTP framework. It supports declarative role/scope/client-based access control and custom guard functions, all built on top of `go.putnami.dev/http` middleware.

## Installation

```go
import "go.putnami.dev/security"
```

The module depends on `go.putnami.dev/http` and uses its `Middleware`, `Context`, `Claims`, and `Response` types.

## Overview

The security package has two main concerns:

1. **Authentication** -- Resolving the identity of the caller via `IdentityResolver`.
2. **Authorization** -- Enforcing access rules via `Middleware`, using either declarative `Options` or a custom `Guard` function.

Both produce standard `phttp.Middleware` values, so they integrate directly with `ServerPlugin.Use()`, which applies the middleware to every route registered on the server.

## Authentication with IdentityResolver

`IdentityResolver` creates middleware that extracts user identity from the request and populates `ctx.User`. It runs early in the middleware chain -- before any authorization middleware -- and does **not** reject unauthenticated requests on its own.

```go
import (
    phttp "go.putnami.dev/http"
    "go.putnami.dev/security"
)

resolver := security.IdentityResolver(func(ctx *phttp.Context) *phttp.Claims {
    auth := ctx.Header("Authorization")
    if auth == "" {
        return nil // no identity
    }
    // Parse and validate the token (JWT, opaque, etc.)
    claims, err := validateToken(auth)
    if err != nil {
        return nil
    }
    return claims
})

server.Use(resolver)
```

When the resolve function returns `nil`, `ctx.User` remains unset. Downstream authorization middleware will then return `401 Unauthorized`.

### User claims

`ctx.User` is a `*phttp.Claims` struct with typed fields for standard OAuth2/OIDC claims:

| Field      | Type       | Description                                       |
|------------|------------|---------------------------------------------------|
| `Subject`  | `string`   | Subject identifier (JWT "sub" claim)              |
| `Issuer`   | `string`   | Token issuer (JWT "iss" claim)                    |
| `ClientID` | `string`   | OAuth2 client identifier                          |
| `Roles`    | `[]string` | User roles                                        |
| `Scopes`   | `[]string` | OAuth2 scopes                                     |
| `Extra`    | `map[string]any` | Additional custom claims                    |

The `Claims` struct provides convenience methods:

```go
user.HasRole("admin")     // check if user has a role
user.HasScope("write")    // check if user has a scope
user.Get("sub")           // access any claim by key (typed fields first, then Extra)
```

To convert from a `map[string]any` (e.g., parsed JWT claims), use `phttp.ClaimsFromMap(m)`:

```go
resolver := security.IdentityResolver(func(ctx *phttp.Context) *phttp.Claims {
    rawClaims := parseJWT(ctx.Header("Authorization"))
    if rawClaims == nil {
        return nil
    }
    return phttp.ClaimsFromMap(rawClaims)
})
```

`ClaimsFromMap` extracts well-known keys (`sub`, `iss`, `client_id`, `roles`, `scope`) into typed fields and puts everything else in `Extra`. Scope and role values are flexible: they can be a `[]string`, a `[]any` containing strings, or a single space-separated string (the OAuth2 convention).

### Opaque token introspection (RFC 7662)

Self-describing tokens (JWTs) are validated offline against a signing key. **Opaque** tokens carry no claims -- they are meaningful only to their issuer, so validating one requires an online call to the issuer's [RFC 7662](https://www.rfc-editor.org/rfc/rfc7662) introspection endpoint. `security.Introspect` provides that as an identity resolver. Constructing it *is* the opt-in: a server that never calls `Introspect` never makes a per-request introspection call.

```go
import (
    "go.putnami.dev/cache"
    "go.putnami.dev/security"
)

server.Use(security.Introspect(security.IntrospectConfig{
    // Discover introspection_endpoint from the issuer's metadata, or set
    // Endpoint directly.
    Issuer: "https://auth.example.com",
    // ClientID/ClientSecret authenticate THIS server to the endpoint, not the
    // token being introspected.
    ClientID:     os.Getenv("INTROSPECT_CLIENT_ID"),
    ClientSecret: os.Getenv("INTROSPECT_CLIENT_SECRET"),
    // Reject tokens not minted for this resource server (optional, recommended).
    Audience: "my-service",
    // Cache successful results to avoid a call per request. nil disables it.
    // Any cache.Cache works — in-process here, or a Redis/Postgres-backed
    // implementation to share results across instances.
    Cache: cache.NewMemoryCache(cache.MemoryConfig{}),
}))
```

An **active** response is mapped to `ctx.User` via `phttp.ClaimsFromMap` (standard claims to typed fields, the rest to `Extra`). An **inactive** response, a transport error, a timeout, or an unexpected status all fail closed -- `ctx.User` stays unset and downstream authorization returns `401`.

Operational characteristics worth knowing:

- **Opt-in network dependency.** No `Introspect` call means no introspection endpoint in the request path. This is why it is a standalone resolver rather than part of `security.Config`.
- **Audience restriction.** When `Audience` is set, an active token whose `aud` (string or array, per RFC 7662) does not include it is rejected — so a valid token minted for another service is not accepted here. The check runs on every resolution, including cache hits, so it is intentionally *not* part of the cache key: a cache shared by resolvers with different audiences enforces each one's policy on read.
- **Pluggable cache.** Pass any `cache.Cache` and you can switch backends without touching call sites: `MemoryCache` per instance, or a Redis- or Postgres-backed implementation to share results across instances. Introspection depends only on the `cache.Cache` interface and passes the request context to every `Get`/`Set`, which network backends need for cancellation and timeouts. `CacheTTL` defaults to 5 minutes and is capped by the token's own `exp`, so a cached result never outlives the token. **Inactive results are never cached**, so a just-issued or just-reactivated token validates on the next call.
- **Concurrent de-duplication.** A burst of requests carrying the same not-yet-cached token is collapsed into a single upstream introspection call; the result is shared (each request still gets its own `Claims`). This holds the auth server's load to the rate of *distinct* tokens even without a cache.
- **Tokens are never stored raw.** The cache key is a SHA-256 digest over the resolved endpoint, configured issuer/client credentials, and token; the token and client secret never sit in a map or on disk in the clear.
- **Fail closed, bounded.** Each request has a `Timeout` (default 5s) and the response body is read with a 64 KiB cap. A `401`/`403` from the endpoint (this server's `ClientID`/`ClientSecret` are wrong) is logged distinctly from an inactive token so a misconfiguration is unambiguous.
- **Throttling is backpressure, not an outage.** A `429 Too Many Requests` is retried once after the endpoint's `Retry-After` (delta-seconds or HTTP-date, capped at 1 s, with a short jittered delay when the header is absent). The retry runs within `ClientAssertionTimeout` and is shared by every request collapsed onto the same token. If the endpoint still throttles, or the remaining budget cannot cover the wait, that request fails closed and logs `token introspection throttled`. A 429 never counts toward the optional circuit breaker (`Breaker`), so a burst of throttled calls cannot open it. A `5xx` or a transport error still counts, including on the retry.
- **Client authentication method.** By default the `ClientID`/`ClientSecret` are sent via HTTP Basic (`client_secret_basic`). Some endpoints instead read them as `client_id`/`client_secret` form parameters; set `ClientAuth: security.ClientAuthPostBody` for those.
- **Observability.** Each outcome increments a counter under the `security.introspect` expvar map (`cache_hit`, `active`, `inactive`, `audience_mismatch`, `client_credentials_rejected`, `endpoint_error`, `breaker_open`, `throttled`), plus `endpoint_call` for actual upstream requests, including the single retry after a 429 — so `endpoint_call` versus the request total shows what the cache and de-duplication saved.
- **`https` required.** Like the JWKS path, plaintext `http` is rejected for non-loopback hosts unless `AllowInsecure` is set.

Because resolvers chain first-win, compose `Introspect` with the others and keep any issuer-specific fast-path (e.g. skipping a token that obviously is not introspectable) in your own resolver ahead of it:

```go
server.Use(security.APIKey(security.APIKeyConfig{Keys: staticKeys}))
server.Use(security.Introspect(introspectCfg))
server.Use(security.JWKSJWT(security.JWKSJWTConfig{Issuer: issuer}))
```

## Authorization with Declarative Options

Use `security.Options` for rule-based access control. Pass it to `security.Middleware()` to get a middleware that checks the current user against the declared constraints.

### Require all roles

The request is allowed only if the user has **every** listed role:

```go
adminOnly := security.Middleware(security.Options{
    Roles: []string{"admin", "editor"},
})

server.Use(adminOnly) // enforced on every route on the server
```

### Require any role

The request is allowed if the user has **at least one** of the listed roles:

```go
staffAccess := security.Middleware(security.Options{
    RolesAny: []string{"admin", "editor", "moderator"},
})
```

### Require scopes

Works the same as roles but checks `Scopes`. `Scopes` requires all; `ScopesAny` requires at least one:

```go
writeAccess := security.Middleware(security.Options{
    Scopes: []string{"read", "write"},
})

readOrWrite := security.Middleware(security.Options{
    ScopesAny: []string{"read", "write"},
})
```

### Restrict by client ID

Limit access to requests from specific OAuth2 clients:

```go
trustedClients := security.Middleware(security.Options{
    Client: []string{"my-app", "partner-app"},
})
```

### Combining constraints

All fields in `Options` are evaluated together. Every non-empty field must pass for the request to proceed:

```go
strict := security.Middleware(security.Options{
    Roles:  []string{"admin"},
    Scopes: []string{"write"},
    Client: []string{"internal-app"},
})
```

This requires the user to have the `admin` role **and** the `write` scope **and** be calling from the `internal-app` client.

## Authorization with Custom Guards

For authorization logic that goes beyond static role/scope checks, use a `Guard` function. A guard receives the typed user claims and the request context, and returns `true` to allow or `false` to deny. Registered with `server.Use`, it runs on every route:

```go
verifiedOnly := security.Middleware(security.Guard(func(user *phttp.Claims, ctx *phttp.Context) bool {
    verified, _ := user.Extra["email_verified"].(bool)
    return verified
}))

server.Use(verifiedOnly)
```

Guards are checked after identity resolution. If `ctx.User` is `nil` (no authenticated user), the middleware returns `401 Unauthorized` before the guard is called.

You can also declare a guard with the explicit `security.Guard` type and pass it to `security.Middleware`:

```go
var verifiedOnly security.Guard = func(user *phttp.Claims, ctx *phttp.Context) bool {
    verified, _ := user.Extra["email_verified"].(bool)
    return verified
}

server.Use(security.Middleware(verifiedOnly))
```

Because middleware applies to every route, a check that depends on a specific path parameter — such as "the caller owns this resource" — is best performed inside the handler, where the route's parameters are unambiguous:

```go
func updateProfile(ctx *phttp.Context) *phttp.Response {
    if ctx.User == nil || ctx.User.Subject != ctx.Param("userId") {
        return phttp.Forbidden()
    }
    // ... update and respond ...
    return phttp.JSON(map[string]string{"updated": ctx.Param("userId")})
}

server.PUT("/users/[userId]/profile", updateProfile)
```

## HTTP Response Behavior

The middleware produces two possible error responses:

| Condition                  | Response          |
|----------------------------|-------------------|
| `ctx.User` is `nil`        | `401 Unauthorized` |
| User exists but rule fails | `403 Forbidden`    |

If all checks pass, the middleware calls `next()` and the request proceeds to the handler.

## Applying Middleware

Authentication and authorization middleware is registered with `server.Use` and runs on every route, in registration order. The identity resolver must come first so `ctx.User` is populated before any authorization rule runs:

```go
server.Use(resolver)
server.Use(security.Middleware(security.Options{Roles: []string{"user"}}))
```

### Exempting public routes

Because a rule applies to every route, use `ExcludePaths` to let specific path prefixes through without authentication (health checks, public assets, and so on):

```go
server.Use(resolver)
server.Use(security.Middleware(security.Options{
    RolesAny:     []string{"user", "admin"},
    ExcludePaths: []string{"/_/", "/public/"},
}))

server.GET("/public/status", statusHandler) // reachable without auth
server.GET("/profile", getProfile)           // requires user or admin
```

Requests whose path starts with an excluded prefix skip the rule entirely; every other route requires the user to satisfy it.

## Best Practices

- **Always register `IdentityResolver` globally** so that `ctx.User` is populated before any authorization middleware runs.
- **Prefer declarative `Options`** over custom guards when the check is a simple role, scope, or client match. Declarative rules are easier to audit and test.
- **Use guards for contextual checks** such as "user owns this resource" where the decision depends on both the user claims and the request parameters.
- **Keep token validation out of authorization middleware.** Parse and validate tokens (JWT signature, expiry, issuer) in the `IdentityResolver`. Authorization middleware should only inspect the resulting claims.
- **Apply the principle of least privilege.** Use `Roles` (require all) rather than `RolesAny` unless you intentionally want a union check.
- **Test middleware in isolation.** Create a `phttp.Context` with `phttp.NewContext(httptest.NewRecorder(), req)`, set `ctx.User` to a `*phttp.Claims`, and call the middleware directly. See the package test file for examples.
