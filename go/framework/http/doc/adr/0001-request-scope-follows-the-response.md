# ADR 0001 — Reconcile the request scope with the response the client receives

- **Status**: accepted
- **Scope**: `go.putnami.dev/http` (`go/framework/http`)

## Context

Request-scoped dependencies can own a transaction or connection. Something must
commit or discard that work exactly once, even on panic or disconnect. A
handler returns a `*Response`, not an error: a 404 is not a failure, a 500 is,
and a direct write to the `ResponseWriter` returns nil. A commit can also fail
after the handler produced a 200.

## Decision

The server owns one request scope per request and reconciles it against the
response the client receives.

A canceled or timed-out request context, or a status of 500 or above, is a
failure and rolls the scope back. Everything else, including every 4xx and a
nil response, commits. A handler that wants a 4xx to discard work marks its
unit of work rollback-only.

Reconciliation runs exactly once. An unconverted panic rolls the scope back,
then re-raises, so outer recovery middleware behaves normally. When the handler
succeeded but the commit failed, the server replaces the response with a
sanitized 500; it never reports success for unpersisted work.

The middleware chain is composed once, before the first request, and reused for
synthesized handlers such as the OPTIONS preflight fallback, so an unmatched
request passes through the same middleware as a registered route.

## Rejected alternatives

- **Roll back only on panic.** A 500 would persist the partial work that
  caused it.
- **Roll back on every 4xx.** Audit rows and login-attempt records written
  with a 401 must be kept.
- **Let each scoped provider pick its commit point.** Providers would disagree,
  and none can see the response.
- **Return the commit error verbatim.** It discloses driver and topology
  detail.
- **Compose middleware per request.** It allocates for nothing and lets the
  preflight path diverge.

## Consequences

- Middleware registered after the first request is ignored; registration is a
  wiring-time activity.
- Scoped providers' rollback and close must be safe on a partially used scope,
  including from the panic path.
- A handler cannot assume the response it returned is the one sent.
