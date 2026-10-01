# ADR 0005 — The service registry is scoped to the application

- **Status**: accepted
- **Scope**: `go.putnami.dev/client` (`go/framework/client`)

## Context

Credentials and streams need an owner that ends them. Without one, a service
token and its refresh goroutine outlive the application that acquired them, two
applications in one process have no stated sharing rule, and a late
acquisition surfaces as an unexplained `context.Canceled`.

## Decision

**One application owns one registry.** `ServicesPlugin` publishes exactly one
`*ServiceBindings` into the application's container. Every module of the
application shares its credential cache; two applications share none. The
package holds no ambient cache: a guard test fails if a package-level variable
acquires credential state or a second place builds a manager.

**`ServicesPlugin` implements `app.Stopper` and not `app.Starter`.** The
registry is built by dependency injection, and the stop phase runs every
`Stopper` in reverse registration order.

**`Close` is the single, idempotent end point.** It cancels in-flight
credential refreshes, drops every cached credential, and closes the stream
sessions tracked on it. A refresh that completes during close neither caches
its value nor surfaces its own cancellation; it returns the closed-registry
error.

**Ownership is explicit.** A built-in token source is framework-owned and ends
with the registry. A caller-supplied `CredentialBinding.Provider` stays
caller-owned: the registry stops calling it and never closes it. A stream
session is tracked, not owned: the transport keeps the terminal, and the
session records `cancel` when the registry closes it without one.

**A late acquisition is typed.** Binding a client, acquiring a credential and
tracking a session after close fail with `client.closed`, which the credential
path passes through unchanged. `client.canceled` and `client.deadline` stay
reserved for the caller's budget.

## Consequences

- A test that stops its application leaves no bearer token and no refresh
  goroutine behind.
- A stream is closed by shutdown only if its transport calls `TrackStream`.
  SSE, Connect and provider-owned WebSocket streams do; the first-party
  WebSocket transport does not.
- The credential manager takes its clock from the registry, so expiry is
  tested in controlled time.
