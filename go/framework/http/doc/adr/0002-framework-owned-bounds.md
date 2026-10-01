# ADR 0002 — Every long-lived resource carries a framework-owned bound

- **Status**: accepted
- **Scope**: `go.putnami.dev/http` (`go/framework/http`)

## Context

`ServerConfig` documents defaults with `default:"…"` tags, which only
`go.putnami.dev/config` applies. A directly constructed
`http.ServerConfig{Port: 8080}`, the shape every example uses, carries Go zero
values instead. A zero `ShutdownTimeout` would give an already-expired drain
context and abandon in-flight requests. `http.Server` read and write timeouts
do not apply to hijacked WebSocket connections or SSE streams, so without an
explicit bound a stalled peer pins a goroutine and a descriptor.

## Decision

Every bound maps its zero value to the package's documented default before use:
`ShutdownTimeout`, `WebSocketIdleTimeout`, `StreamWriteTimeout`,
`ReadTimeout`, `WriteTimeout`, `MaxHeaderBytes`, and `MaxBodySize`. A directly
constructed config and a loaded one behave identically.

Where opting out is legitimate, the opt-out is a negative value, never zero. A
negative `ShutdownTimeout` drains under the caller's context alone; a negative
`WebSocketIdleTimeout` or `StreamWriteTimeout` disables that deadline.

A stream clears the server-wide write deadline and re-arms a per-write one.

## Rejected alternatives

- **Apply tag defaults in `NewServerPlugin`.** It couples http to the config
  tag reader and still cannot tell a loaded zero from unset.
- **Require loading through `config.Load`.** No example does it.
- **Zero means unbounded.** It breaks shutdown drain and leaks a goroutine per
  client that stops reading.
- **Bound streams with the server-wide `WriteTimeout`.** It kills every
  long-lived stream at the first tick.

## Consequences

- Each new bound needs its own zero-value resolution; a `default:` tag alone is
  a contract gap.
- Each default lives in the struct tag and a package constant; a package test
  asserts they agree.
