# ADR 0003 — WebSocket admission runs the security chain on the rebuilt request

- **Status**: accepted
- **Scope**: `@putnami/application` (`typescript/framework/application`)

## Context

A browser `WebSocket` cannot put a credential on the upgrade, so the first
client frame, `init`, is the admission channel
([clientcontract ADR 0002](../../../../../protocols/clientcontract/doc/adr/0002-websocket-admission-uses-a-first-frame-state-machine.md)).
The endpoint's security chain reads an HTTP request, and the bare upgrade
carries none of what the caller sent. Running the chain on it refuses every
conforming first-party consumer. A second, WebSocket-only authorization path
would drift from the chain. The Go provider makes the same choice.

## Decision

The security chain stays the authority, unchanged. Only the request it reads is
rebuilt.

On the `init` frame the dispatcher builds a `Request` from the upgrade URL and
method plus the headers the frame declared: each credential on the header its
profile names (`Authorization` for `service-token` and `forwarded-user-token`,
`profile.header` for `api-key` and `named-header`), `X-Client-Id` from the
frame's identity, the propagation members and the ordinary headers. The identity
resolvers captured at the upgrade run against it, then the endpoint's middleware
chain, and only then does `ready` leave.

Before sending `ready`, the dispatcher re-reads the conversation. If the caller
cancelled, its budget elapsed, or the application started draining during an
asynchronous credential check, `ready` is not sent.

The provider checks only what the chain cannot see: the frame addresses this
route's operation; its encoding is one this provider decodes; every named
credential profile is declared; the provided profiles satisfy exactly one
declared security alternative; no ordinary header shadows a declared credential
header; the client identity is in the declared allow-list when there is one.

## Consequences

- A route under a first-party client contract answers `Sec-WebSocket-Protocol:
  putnami.service.v1` and refuses an upgrade offering only tokens it cannot
  speak. A route without such a contract keeps the raw JSON bridge.
- The wire's refusal order comes first: a resume request on a transport that
  does not declare resume is refused with `client_contract.invalid_resilience`
  before an unsupported encoding is refused with
  `client_contract.invalid_transport`.
- A refused admission is a typed `error` frame (status, stable code, message,
  declared details) and a `1008` close. The close reason is a fixed framework
  string and never carries a message, detail body or credential.
- The contract's `ValidateWebSocketInitForOperation` is not called. It needs a
  projected `OperationV1` this package could only rebuild by duplicating the
  OpenAPI security projection, which is the second authorization source this
  decision avoids.
