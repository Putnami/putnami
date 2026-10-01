# go.putnami.dev/security

Authentication (JWT/JWKS/API key/RFC 7662 introspection identity resolution)
and authorization (role/scope/client checks, custom guards) middleware for
`go.putnami.dev/http`.

All examples import the framework HTTP package as `phttp`:

```go
import (
    phttp "go.putnami.dev/http"
    "go.putnami.dev/security"
)
```

## Quick Start

```go
// Resolve identity from a JWT validated against a JWKS/OIDC issuer.
server.Use(security.JWKSJWT(security.JWKSJWTConfig{
    Issuer:   "https://accounts.google.com",
    Audience: "my-service",
}))

// Then enforce role requirements on every route.
server.Use(security.Middleware(security.Options{
    Roles: []string{"admin"},
}))
```

Both calls register global middleware via `server.Use`; the identity resolver
must be registered before any authorization rule.

## Authentication

Each resolver populates `ctx.User` and returns `nil` (leaving `ctx.User` unset)
when no valid credential is present.

```go
// HMAC (HS256) bearer tokens. exp is required by default; set
// AllowMissingExpiration to accept tokens without it.
server.Use(security.JWT(security.JWTConfig{
    Secret:   os.Getenv("JWT_SECRET"),
    Audience: "my-service",
    Issuer:   "https://auth.example.com",
}))

// JWKS / OIDC (RS256, ES256, ES384). https is required for key endpoints
// unless AllowInsecure is set (loopback is always allowed). exp is required by
// default; set AllowMissingExpiration to accept tokens without it.
server.Use(security.JWKSJWT(security.JWKSJWTConfig{JWKSURL: "https://issuer/.well-known/jwks.json"}))

// Static API keys (constant-time compared).
server.Use(security.APIKey(security.APIKeyConfig{
    Keys:   []string{os.Getenv("API_KEY")},
    Scopes: []string{"ingest"},
}))

// Opaque bearer tokens via RFC 7662 introspection. Opt-in: no call => no
// per-request introspection. Cache is pluggable via any go.putnami.dev/cache
// (nil disables it) — MemoryCache, or a Redis/Postgres-backed cache.Cache to
// share across instances; the request context is passed to Get/Set. Inactive
// results are never cached and CacheTTL (default 5m) is capped by the token's
// exp. Cache keys are scoped to endpoint and client credentials, with the
// token hashed instead of stored raw. Audience (when set) rejects tokens whose
// aud excludes it (re-checked on cache hits, so it is not in the cache key).
// Concurrent requests for the same uncached token collapse to one upstream
// call. Outcomes are counted under the security.introspect expvar map.
// Transport errors/timeouts fail closed. A 429 is backpressure, not an outage:
// it is retried once after Retry-After (capped at 1s, within the leader's
// budget, shared by the collapsed cohort); a 429 that persists fails that
// request closed (counted as throttled) and never trips the optional Breaker.
// ClientID/ClientSecret authenticate THIS server to the endpoint, not the
// token, via HTTP Basic by default (ClientAuth: ClientAuthPostBody sends them
// as body params instead). https required for non-loopback unless
// AllowInsecure.
server.Use(security.Introspect(security.IntrospectConfig{
    Issuer:       "https://auth.example.com", // or Endpoint: "https://auth.example.com/introspect"
    ClientID:     os.Getenv("INTROSPECT_CLIENT_ID"),
    ClientSecret: os.Getenv("INTROSPECT_CLIENT_SECRET"),
    Audience:     "my-service",
    Cache:        cache.NewMemoryCache(cache.MemoryConfig{}),
}))
```

## Security Options

```go
security.Middleware(security.Options{
    Roles:        []string{"admin"},           // all required
    RolesAny:     []string{"editor", "admin"}, // at least one
    Scopes:       []string{"read", "write"},   // all required
    ScopesAny:    []string{"read"},            // at least one
    Client:       []string{"web-app"},         // allowed client IDs
    ExcludePaths: []string{"/_/"},             // prefixes that skip the rule
})
```

Returns `401` when `ctx.User` is unset, `403` when a rule fails.

### Optional authentication

`Optional: true` serves a request that presents no credential, for a route that
answers anonymous and authenticated callers alike (a public package read that
answers more to an authorized caller). Every other field then applies to an
authenticated caller only:

```go
api.Endpoint("GET", "/{namespace}/{package}/resolve").
    Secure(security.Options{Optional: true, Scopes: []string{"packages:read"}}).
    Handle(resolve) // ctx.User is nil for an anonymous caller
```

| Request | Answer |
|---|---|
| No identity, and `Authorization` absent or empty | Served, `ctx.User` is nil |
| An identity that satisfies the rule | Served |
| An identity that fails the rule | `403` |
| A non-empty `Authorization` header that resolved no identity | `401`, never served anonymously |

`Options` reports the choice through `phttp.SecurityOptionalAuthentication`
(`OptionalAuthentication() bool`), which a custom rule may implement too. It is
the only claim that lets a first-party client contract offer an anonymous
alternative (`go/framework/api/AI.md`). A server-level or module-level rule that
requires authentication still runs first. See
`doc/adr/0002-an-optional-rule-serves-a-caller-that-presents-no-credential.md`.

## Custom Guards

A `Guard` receives the user claims and request context and returns whether to
allow the request. Wrap it with `security.Middleware` and register it globally:

```go
server.Use(security.Middleware(security.Guard(func(user *phttp.Claims, ctx *phttp.Context) bool {
    verified, _ := user.Extra["email_verified"].(bool)
    return verified
})))
```

Checks that depend on a path parameter (e.g. resource ownership) are best done
inside the handler, where the route's parameters are unambiguous.

## Identity (Claims)

```go
func handler(ctx *phttp.Context) *phttp.Response {
    if !ctx.IsSecured() {
        return phttp.Unauthorized()
    }
    return phttp.JSON(map[string]any{
        "user":   ctx.User.Subject,
        "roles":  ctx.User.Roles,
        "scopes": ctx.User.Scopes,
    })
}
```

## Plugin

`security.NewPlugin(server, security.Config{...})` wires `JWKSJWT` + `Options`
onto the server during `Configure`. If neither `JWKSURL` nor `Issuer` is set it
fails closed (every non-excluded request gets `401`) unless
`Config.AllowUnauthenticated` is set.

See `doc/getting-started.md` for the full reference.

## Contract invariants

- Invalid, incomplete, unavailable, or ambiguous authentication state fails
  closed before authorization or handler execution.
- Missing identity maps to 401, unless the rule declares `Optional` and the
  request's `Authorization` header is absent or empty; an authenticated
  identity that lacks authority maps to 403.
- Logs, errors, counters, and public key documents never disclose bearer tokens,
  client secrets, password material, or private signing parameters.

Support is **stable** in `../../../putnami.support.json`; the normative feature
contract is `specs/authentication-authorization.json`, with the decisions in
`doc/adr/0001-fail-closed-authorization-decisions.md` and
`doc/adr/0002-an-optional-rule-serves-a-caller-that-presents-no-credential.md`.
