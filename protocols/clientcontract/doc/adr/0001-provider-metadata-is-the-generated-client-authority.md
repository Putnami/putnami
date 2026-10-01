# ADR 0001 — Provider metadata is the generated-client authority

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/clientcontract`

## Context

OpenAPI and Proto publish portable wire contracts, but neither records the
whole operational contract a Putnami consumer needs: service identity and
audience, credential intent, transport order, framework error codes,
idempotency and bounded resilience. Each consumer reconstructing those
semantics on its own drifts from the others.

OpenAPI also cannot reconstruct protobuf field numbers, numeric enum values,
oneofs, maps or presence. Deriving them from JSON property order would yield
valid-looking clients with a different binary wire contract.

## Decision

Version 1 `x-putnami-client` metadata has a document shape and an operation
shape. The document identifies the service, value-free credential profiles,
defaults, and a deterministic client-useful protobuf descriptor projection.
Each operation records stream cardinality, ordered transports, security and
authorization requirements, declared errors, idempotency and resilience
overrides.

The marker makes a first-party contract strict. Unknown fields, unsupported
schema keywords, incomplete metadata, invalid references and inconsistent
transport declarations are errors. Credential values stay outside the protocol
and generated artifacts.

This package does not project provider declarations, generate a client,
resolve credentials or execute a transport. Each consumer adopts the versioned
protocol explicitly and proves conformance against its corpus.

## Consequences

- Go and TypeScript readers consume the same versioned corpus.
- The provider authors client semantics.
- Transport fallback and security-alternative order stay deterministic and
  reviewable.
- Protobuf metadata keeps exact binary field identity.
- A new meaningful field or closed enum value needs a protocol-version decision
  and coordinated reader support. A strict reader refuses an unknown member, so
  a stale reader fails closed.
