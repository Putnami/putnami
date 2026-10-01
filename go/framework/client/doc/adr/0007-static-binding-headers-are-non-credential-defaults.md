# ADR 0007 — Static binding headers are non-credential defaults

- **Status**: accepted
- **Scope**: generated Go and TypeScript service clients

## Context

A generated client needs to send a constant advisory value, such as
`X-Putnami-Observed-Revision`, beside a per-call forwarded bearer. An advisory
value must not need a credential profile or a hand-written transport.

## Decision

This record owns the shared rule; the TypeScript surface is
[`@putnami/client` ADR 0006](../../../../../typescript/framework/client/doc/adr/0006-static-binding-headers-are-non-credential-defaults.md).

The configuration spelling is `clients.services.<service>.headers`: in Go
`ServiceBinding.Headers` (`map[string]string`), in TypeScript
`ServiceBinding.headers`. The values are non-secret request defaults. The
registry and each client keep snapshots.

**Validation.** Names are canonicalized. Case aliases, invalid names,
reserved headers and every provider-declared credential header fail
construction with `client.config`; messages never contain values. Reserved
headers are authentication, identity, request-context, tracing and transport
headers, including `X-Forwarded-For`, `Forwarded`, `X-Real-IP`,
`X-Cloud-Trace-Context`, `Origin`, and `X-Trace-Id`, `X-Region`,
`X-Experiments` (which the TypeScript runtime propagates from the inbound
request). A value is visible ASCII with inner spaces or tabs and none at
either end, because Fetch cannot send other bytes as `net/http` does and trims
surrounding whitespace.
[`fixtures/binding/headers.json`](../../../../../protocols/clientcontract/fixtures/binding/headers.json)
pins the accepted, canonical and refused maps and the reserved set for both
runtimes.

**Application.** An explicit operation header wins case-insensitively, even
when empty. A binding that supplies the operation's idempotency key fails
before dispatch or cache lookup. Defaults enter the cache's request projection
before key calculation and stay across retries without affecting credential
selection. Transport preparation owns a private request copy, so identity,
idempotency and retry mutations never change the caller's request or the cache
input.

**Carriers.** REST, Connect, SSE and provider-owned WebSocket send the values
as request headers. First-party WebSocket sends them in the init frame's
ordinary headers, apart from credentials, and keeps the handshake free of
application headers.

## Consequences

- Header values are ordinary configuration and are not redacted; credentials
  belong in `Credentials`.
- No provider contract or wire member is added.
