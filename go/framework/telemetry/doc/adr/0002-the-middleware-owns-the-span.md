# ADR 0002 — The HTTP middleware owns the span's end, whatever happens downstream

- **Status**: accepted
- **Scope**: `go.putnami.dev/telemetry` (`go/framework/telemetry`)

## Context

A handler can return a response, return nil after writing directly, or panic,
and the workload decides where recovery middleware sits. A span never ended
holds memory and never reaches the backend, so the failed request vanishes
from the trace. Handlers often run in under a millisecond.

## Decision

The middleware ends its span on every path.

A panic is recovered just long enough to mark the span errored, with a panic
attribute and a 500 status, and to record request metrics; then it is
re-raised. Outer recovery middleware behaves as without tracing, so the
middleware is safe outermost. This recover/re-panic pair must stay.

A nil response ends the span normally; a server-error status marks it errored.

Durations are recorded in milliseconds as floating-point values, with
sub-millisecond precision.

Inbound W3C trace context is extracted before the span starts, so the span
continues the caller's trace.

## Rejected alternatives

- **`defer` the end and let the panic pass.** The span ends unerrored and
  without metrics.
- **Convert the panic to a 500.** It overrides the workload's recovery
  middleware.
- **Require tracing inside recovery.** An ordering rule invisible until a
  production panic.
- **Whole-millisecond durations.** Most handlers would record zero.

## Consequences

- A workload without recovery middleware still crashes the request, as it
  would without tracing.
- Dashboards must read durations as floating-point milliseconds.
