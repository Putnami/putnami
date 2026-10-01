# ADR 0002 — Bound the server by default, mount the gateway explicitly

- **Status**: accepted
- **Scope**: `go.putnami.dev/grpc` (`go/framework/grpc`)

## Context

`grpc.NewServer()` with no options keeps connections forever, tolerates
aggressive client pings, and does not cap concurrent streams. `GracefulStop`
waits for every in-flight RPC, so one long stream blocks a rolling restart.
Gateway handlers are mounted by `RegisterOn(httpServer)`, not by the
lifecycle, so `Use(gateway)` alone starts cleanly and answers 404 to every
Connect call.

## Decision

The server applies keepalive parameters, a keepalive enforcement policy, and a
concurrent-stream cap before any caller `ServerOption`. gRPC options are
last-wins, so an explicit `WithServerOption(...)` still overrides a default.

Server reflection is opt-in (`WithReflection(true)`).

`Stop` runs `GracefulStop` under a deadline that has a default, and force-stops
the server when it elapses.

Mounting stays explicit. A gateway that holds handlers but was never passed to
`RegisterOn` logs a warning at `Start`.

## Rejected alternatives

- **Append defaults after caller options.** It silently overrides explicit
  configuration.
- **Reflection on by default.** Every deployment would publish its service
  catalogue.
- **Mount gateway handlers from `Start`.** The gateway would depend on the HTTP
  server and register routes after its middleware chain is composed.
- **Unbounded `GracefulStop`.** A stuck stream would block a rolling restart.

## Consequences

- A workload that wants unbounded connections says so explicitly.
- The forced stop can cut an in-flight stream.
- `Use(gateway)` alone stays legal, so the warning, not a failure, catches the
  missing `RegisterOn`.
