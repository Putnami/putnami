# ADR 0014 — A client drops a response property it does not declare

- **Status**: accepted
- **Scope**: both client runtimes (`go.putnami.dev/client`, `@putnami/client`):
  success bodies, stream messages and frames, and declared error details

## Context

A generated schema marks every object `additionalProperties: false`. Adding an
optional response property is a compatible change for a provider. If a client
refuses any response carrying an undeclared property, then in a rolling deploy
where the provider rolls first, every consumer still running the earlier client
reads every new answer as a contract violation, and the dependency is down
until every consumer is regenerated and redeployed.

## Decision

**A client reading a document the provider sent drops every property a closed
object does not declare, and validates every declared property as strictly as
before.**

1. **Where it applies.** A success body, a server-stream message (SSE,
   WebSocket, Connect), a provider-owned WebSocket frame, and the `details` of
   a declared error: documents the provider authors.
2. **Where it does not.** A request body, a client-stream message and a frame
   the client sends stay strict: an undeclared property is refused before
   anything is sent, because it is a bug in the caller.
3. **Declared properties keep every check**: type, format, `required`,
   `nullable`, enum, bounds, pattern, array shape and nesting. A wrong-typed
   declared property is refused whether or not added properties are present.
4. **A dropped property is absent from the result.** The TypeScript decoder
   and the Go stream readers return the projection. A Go unary call decodes the
   provider's bytes into the generated struct, which binds only declared
   properties; an opaque member keeps its bytes
   ([ADR 0008](0008-opaque-json-is-a-declaration.md)). In error details, the
   property is dropped before redaction and never reaches
   `RemoteError.Payload` or the TypeScript error's `details`.
5. **A union selects as before, then once more.** A value that matches a
   variant of a `oneOf` without a discriminator as sent selects exactly as the
   strict rule does. Only a value that matches no variant as sent is projected
   again without undeclared properties, and it must then match exactly one
   variant. An added property that makes two variants match is refused. A
   discriminated union selects by its tag and is unaffected.
6. **Byte-for-byte sinks are unchanged.** `WithSuccessBody` and the TypeScript
   success-body sink deliver the bytes the provider sent, because a digest over
   those bytes is their purpose. The raw `Response` of the low-level Go
   `DoOperation` is unchanged too.

## Consequences

- A provider can add an optional response property and deploy before its
  consumers. Removing a property, making an optional one required, or changing
  a declared type stays breaking.
- A property a provider sends by mistake is no longer caught by its consumers;
  the provider's own tests catch it.
- A new discriminator value is a new variant, not an added property: an earlier
  client still refuses it.
