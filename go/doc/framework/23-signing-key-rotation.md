# Signing key rotation

`go.putnami.dev/security` owns signing and verification. Its `KeyringStore`
seam carries a serializable private owner document, while the separate
`go.putnami.dev/keyringstore` package provides durable Postgres persistence
without adding a database dependency to the security module.

`go.putnami.dev/keyringstore` is **stable** and owned by the Go SDD surface. Its
signing-key-rotation specification and accepted atomic overlap-rotation decision
record live next to the package source.

## Rotation invariant

A rotation is one transaction:

1. compare-and-set the current active key to `retiring`;
2. insert the active successor;
3. commit both changes, or roll both back.

Not-found, conflict, and retryable serialization outcomes leave the durable
owner unchanged. A duplicate successor cannot commit a half-rotation with no
active replacement.

During the overlap window, new tokens use the successor while public JWKS
contains both the active and retiring keys. Revocation ends verification for the
retiring key. Keep overlap at least as long as the maximum issued-token lifetime
plus clock skew.

## Durable store

```go
store, err := keyringstore.New(pool, keyringstore.Config{
    KeyringID: "issuer-primary",
})
if err != nil {
    return err
}

provider, err := security.NewSigningKeyProviderFromStore(
    ctx,
    store,
    security.ProviderConfig{},
)
```

The row state is authoritative when loading. Only active and retiring rows are
publishable. Private JWK material is required by the signing owner and is never
included in errors or logs; encryption at rest is an explicit deployment
responsibility.

## Scheduled policy

`keyringstore.RotationScheduler` invokes a caller-supplied policy at a fixed
interval. It never invents keys or rotates on its own. The returned stop
function is idempotent and waits for an in-flight policy call to exit; scheduled
calls do not overlap.

## Operational checks

- Run more than one signer only against a shared durable store.
- Treat an empty durable store as a startup failure unless ephemeral operation
  was explicitly selected for development.
- Alert on non-applied rotation outcomes and preserve the predecessor until the
  verification overlap ends.
- Never put private JWK fields, bearer tokens, or credentials in diagnostics.

## Related guides

- [Security](/docs/frameworks/go/security) — authentication and authorization
- [Persistence](/docs/frameworks/go/persistence) — pool and transaction setup
- [Platform endpoints](/docs/frameworks/go/platform-endpoints) — health and operational evidence
