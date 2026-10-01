# ADR 0006 — A declared error's schema describes `details`, not the envelope

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/clientcontract`, both first-party clients
  and both first-party provider projections

## Context

[ADR 0003](0003-stable-error-codes-are-the-go-framework-vocabulary.md) fixes the
first-party error envelope `{code, error, message, details?}`. A client must
know what `x-putnami-client.errors[].schema` describes. If one client validates
it against the whole body and another against `details`, a well-formed error
from one provider is refused as a contract violation by the other client
(`client.response`): a hard rejection of a valid response.

The envelope's `message` is provider prose. A consumer that only logs or
forwards a failure must not widen what it exposes, and one that shows the
failure to the person who made the request needs the text.

## Decision

**`errors[].schema` describes the `details` member and nothing else.** The
envelope is a contract constant. Re-declaring `code`, `error` and `message`
per error would let a provider contradict ADR 0003 in its own contract.

1. **A client validates the declared schema against `details`.** An absent or
   non-conforming `details` where a schema is declared is a contract violation
   (`client.response`).
2. **The generated typed payload is `details`.** Envelope fields reach the
   caller through the framework error object (`RemoteError.Code()` and
   `RemoteError.StatusCode` in Go, `ClientFrameworkError.code` and `.status` in
   TypeScript), never through the generated payload type.
3. **An error with no details declares no schema.** The client then produces
   the typed error with no payload.
4. **Providers declare details per code.** Go uses
   `MayThrowDetails(code, api.Type[T]())`: `T` is that code's schema and
   nothing else is. A Go `Throws` schema documents the whole response body and
   never becomes a details schema. TypeScript uses
   `.mayThrowDetails(code, schema)`. Its `.throws(status, …, schema)` form is
   read as the details of every declared code at that status that declares
   none of its own.
5. **`message` is carried only on opt-in.** The service binding option
   `carryRemoteMessage` (`client.ServiceBinding.CarryRemoteMessage` in Go;
   `ServiceBinding.carryRemoteMessage` and
   `clients.services.<id>.carryRemoteMessage` in TypeScript) sits with `url` and
   `allowInsecure`: the deployment decides which provider's prose this consumer
   may show. Unset, `RemoteError.Message` is empty and
   `ClientFrameworkError.message` is a local synthetic line.
6. **A carried message is redacted of the call's own credential material.**
   Every occurrence of a secret, raw, in standard base64 or in raw URL base64,
   is replaced inline by `[REDACTED]`. A message that is itself one base64
   value whose bytes disclose a secret is replaced whole. Both runtimes apply
   these two rules and no other. Free text has no schema to break, so inline
   substitution is safe; a structured `details` scalar is dropped or replaced
   whole to keep its declared shape. The opt-in adds consent, not trust:
   providers already gate `message` to client-safe text.
7. **`error` is never carried.** Every first-party writer sets it to the status
   text, which the caller has from the status.
8. **The SSE terminal is the same envelope plus `status`**:
   `{status, code, error, message, details?}`, flat, written identically by both
   providers and read by both clients under the same schema, opt-in and
   redaction rules.

## Consequences

One cross-language fixture body is read by both clients:

```json
{"code":"not_found","error":"Not Found","message":"widget 4f0c… does not exist",
 "details":{"resource":"widget","metadata":{"tenant":"acme"}}}
```

Go yields a `*RemoteError` with `Code() == "not_found"`, status 404 and
`Payload` equal to `details`. TypeScript yields a `ClientFrameworkError` with
the same code, status and `details`. `go/framework/client/remote_error_test.go`
and `typescript/framework/client/test/runtime/errors.test.ts` pin both on the
corpus schema.

`details` is the only part of an error body with a schema, so it is the only
part redaction can reason about structurally.
