# ADR 0003 — Stable error codes are the Go framework vocabulary

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/clientcontract`, both first-party providers

## Context

A generated client reads `body.code` to pick the declared error to raise. When
the code is missing or spelled differently from the contract, the client falls
back to matching on status alone. Two declared errors often share a status
(at 400 the implicit `http.bad_request` sits beside any declared validation
error), so the match is ambiguous and the typed error collapses into a generic
`client.remote`. Declared server errors reach the caller as typed errors only
if the contract, the IR, the descriptors and the wire use one vocabulary.

## Decision

- **The canonical vocabulary is `go/framework/errors`**: dotted snake_case codes
  (`http.bad_request`, `not_found`, `internal`). They are the only codes in
  `x-putnami-client`, the IR, protobuf descriptors and the wire.
- **The first-party error envelope is `{code, error, message, details?}`** in
  both languages. The TypeScript dispatcher emits it for every first-party
  endpoint. Non-first-party routes keep the `{statusCode, message, error}` body.
- **PascalCase `ErrorResponseCode` is the TypeScript authoring vocabulary**
  (`.mayThrow('NotFound')`), not a wire identifier. An exhaustive
  `Record<ErrorResponseCode, string>` maps it to the stable code, so a union
  member without an entry is a compile error, not a silent fallback.
- **The implicit pair follows Go**: `{400, http.bad_request}` and
  `{500, http.internal_server}`, because that is what `FromStatus` produces.
- **A declared error without a stable code is a strict generation error.** A
  `.Throws(status)` with no `.MayThrow(code)` names the operation and the
  remedy instead of projecting an empty code.
- **No `errors.` prefix.** No runtime writes it. A guard test fails if it
  appears anywhere in the corpus.

## Rejected alternatives

- **Align Go on PascalCase.** It rewrites the error body of every existing Go
  service. Aligning TypeScript changes only its first-party endpoints.

## Consequences

A generated client selects a declared error by code, so two errors that share a
status stay distinguishable. Go services keep the bytes they send.
