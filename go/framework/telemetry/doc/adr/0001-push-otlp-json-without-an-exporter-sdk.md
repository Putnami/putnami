# ADR 0001 — Push OTLP/JSON directly instead of depending on an exporter SDK

- **Status**: accepted
- **Scope**: `go.putnami.dev/telemetry` (`go/framework/telemetry`)

## Context

Putnami workloads are serverless-first. A container that exits cannot be
scraped, so telemetry is pushed and flushed on the way out. An OTLP exporter
SDK brings a gRPC stack and a large dependency set into every workload, and
fixes the credential model, while many collectors sit behind identity-aware
proxies that need a per-request token. A failing collector must never become
the workload's problem.

## Decision

The package renders OTLP/JSON itself, against the shared
[telemetry protocol](../../../../../protocols/telemetry/README.md), and POSTs it
over HTTP to the collector's `/v1/metrics`, `/v1/traces`, and `/v1/logs`. It has
no OTLP exporter SDK dependency.

`Config.OTLP` fills `MetricReader` and `TraceExporter` only when they are nil;
an explicit reader or exporter wins.

Credentials come from an injected `HTTPClient` whose transport authenticates
each request. The injected client is used verbatim for all three signals, so
its caller owns its timeout; the package timeout configures only the built-in
client.

Export failures are dropped, at most logged, never returned to the caller. The
log sink batches records and flushes on a background ticker and on close, so
logging never blocks on the network.

The metric reader flushes periodically and once more at provider shutdown.
Shutdown is bounded by the configured timeout, and tracer and meter shutdown
errors are reported together.

An OTLP configuration with an empty endpoint fails at configure.

## Rejected alternatives

- **An OTLP exporter SDK** and **scrape endpoints**: rejected by
  [telemetry protocol ADR 0001](../../../../../protocols/telemetry/doc/adr/0001-otlp-json-is-the-only-transport.md)
  and [platform protocol ADR 0001](../../../../../protocols/platform/doc/adr/0001-no-metrics-scrape-endpoint.md).
- **Return export errors.** Observability could fail requests.
- **Flush only on the ticker.** A container exiting within one tick loses its
  last batch.
- **Default the endpoint to localhost.** A misconfiguration becomes silent
  data loss.

## Consequences

- An OTLP wire change is a change here, aligned with the protocol fixtures.
- Only OTLP over HTTP with JSON is supported; a gRPC-only collector needs a
  gateway.
- A misconfigured collector is invisible in the workload; confirm delivery on
  the collector side.
- Logs are opt-in: attaching the sink to the application logger is an explicit
  line in the workload.
