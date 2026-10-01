# ADR 0008 — An operation with several success statuses returns the status it answered

- **Status**: accepted
- **Scope**: `go.putnami.dev/api`, `go.putnami.dev/client`,
  `go.putnami.dev/protocol/clientcontract`, `@putnami/clientgen`, `@putnami/client`

## Context

A provider often answers one operation with several success statuses (`200`
or `201`, `200` or `202`, `200` or `204`), and the status is part of the
answer. The TypeScript emitter returns a union discriminated by `status`. Go
has no discriminated union.

## Decision

**1. The Go method returns `*<Method>Result`, carrying the status.**

- `Status int` is the declared success status the provider answered.
- `Body *T` holds the body when every body-carrying status declares the same
  schema; it is nil on a bodyless status.
- Otherwise each body-carrying status gets `Body<status> *T`, non-nil only for
  the status answered.

A body is always a pointer. Schema equality compares the declaration's
canonical JSON, so one contract always emits the same bytes.

**2. The runtime decides; the method only reads.**
`client.CallOperationResponse` uses the same dispatch, response cache,
credentials, breaker and retries as `CallOperation`, and refuses an undeclared
status, a body on a bodyless status and a body its schema rejects. The method
decodes with `client.DecodeResponse`, which selects the answered status's
schema.

**3. It is refused only where the result cannot travel:** an operation
dispatched on Connect (one status), and a raw octet response. Octets in the
request do not prevent it.

**4. An operation the Go emitter cannot represent can be left out, by name.**
`go.omitOperations` (`GoClientOptions.OmitOperations` in `api.Clients`,
`go: { omitOperations }` in `clientGenerator`) leaves each named operation out
of the Go target. By default an unrepresentable operation fails generation.
Each omission appears in the generated client's doc comment, the build output
and the ownership manifest's `omittedOperations`. A name the contract does not
declare fails. The omission lives in the committed manifest because the
workspace guard judges committed bytes alone. An omitted operation stays
required in workspace coverage and is never counted as covered.

## Consequences

- Changing one body-carrying status's schema moves the Go result from `Body` to
  `Body<status>`: a breaking change visible in the regenerated diff.
- A strict reader that predates `omittedOperations` refuses a manifest that
  carries it; only a target using the option writes it.
