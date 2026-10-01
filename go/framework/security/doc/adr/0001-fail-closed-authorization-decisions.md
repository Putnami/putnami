# ADR 0001 — Separate identity resolution from fail-closed authorization decisions

- **Status**: accepted
- **Scope**: `go.putnami.dev/security` (`go/framework/security`)

## Context

Credential parsing and access policy answer different questions. Resolution
fails on an absent, invalid, expired, or foreign token, or an introspection
timeout. Authorization fails on roles, scopes, client identity, or a guard.
Conflating them blurs 401 and 403 and can turn missing configuration into
public access. Diagnostics and caches can also retain bearer tokens or private
keys.

## Decision

Resolvers validate credentials and populate `ctx.User`; they never grant
access. JWT/JWKS validation enforces the configured algorithm, expiration,
issuer, and audience. Opaque token introspection is opt-in, bounded by a
deadline and a body size, caches only active results, and keys the cache by a
digest, never the raw token.

Authorization runs after resolution. No identity returns 401. An identity that
fails any declared role, scope, client, or guard condition (all or any)
returns 403. Excluded paths are explicit prefixes. A plugin with no resolver
rejects every non-excluded path unless `AllowUnauthenticated` is set, even
when earlier middleware populated a user.

Decision telemetry uses bounded reason codes and public subject identifiers.
Public JWKS, signing evidence, metrics, and string renderings exclude private
JWK parameters. Secret verification uses versioned formats and constant-time
comparison.

## Rejected alternatives

- **Authorize inside each parser.** Combining API keys, JWT, and introspection
  becomes order-dependent.
- **Unconfigured plugin as a no-op.** A missing issuer publishes protected
  routes.
- **403 for every failure.** Clients and monitoring lose the authentication
  boundary.
- **Raw tokens as cache keys or log fields.** Caches and logs become
  credential stores.

## Consequences

- Resolvers must precede authorization in the middleware chain.
- `AllowUnauthenticated` and excluded paths are security-reviewed exceptions.
- Applications own contextual policy and test guards against route data.
- Key persistence comes through the `KeyringStore` seam from a separate module,
  so this package stays database-free.
