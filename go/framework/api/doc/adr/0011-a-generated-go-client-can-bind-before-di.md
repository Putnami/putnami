# ADR 0011 — A generated Go client can bind before DI

- **Status**: accepted
- **Scope**: `go.putnami.dev/api` and `go.putnami.dev/client`

## Context

A config Source loads before the application container exists, so it cannot
resolve a generated client through DI. The low-level constructor leaves
authentication and resilience to the caller, and the explicit-binding runtime
needs the generated package's private service descriptor.

## Decision

Each first-party Go client exports `New<ClientName>Binding(binding
client.ServiceBinding) (*ClientName, error)`. It passes the binding and the
private descriptor to `client.NewServiceClientBinding` and wraps the result
with the typed constructor, which the registration override also uses.

The descriptor stays private: callers supply deployment values, and the
provider keeps owning security, resilience, schemas and typed errors. Each
explicitly constructed client has its own registry and credential cache; there
is no ambient registry. The wire contract and TypeScript registration are
unchanged.

## Consequences

Config Sources own their initial binding and any consumer-specific fallback.
Ordinary consumers keep `Register<ClientName>`, so their clients share the
application's registry and lifecycle.
