# Telemetry

`go.putnami.dev/telemetry` provides OpenTelemetry integration for the Putnami Go framework. It exposes tracing, metrics, HTTP instrumentation, and structured error bridging as an application plugin.

## Overview

The telemetry module wraps OpenTelemetry's tracing and metrics APIs into a plugin that integrates with the Putnami application lifecycle. When the plugin starts, it registers a `TracerProvider` and `MeterProvider` in the DI container and sets them as the OpenTelemetry globals. When the application stops, it flushes pending telemetry data and shuts down exporters gracefully.

Key capabilities:

- **Tracing** -- create spans, propagate context, record errors
- **Metrics** -- counters, histograms, gauges, up-down counters
- **HTTP middleware** -- automatic request tracing and duration metrics
- **Error bridge** -- automatic `errors_total` counter incremented on every structured error

## Installation

```bash
go get go.putnami.dev/telemetry
```

## Plugin Setup

Register the telemetry plugin with your application. Putnami is serverless-first,
so telemetry is **pushed** to a collector: set `Config.OTLP` and the plugin
auto-wires the built-in OTLP/JSON-over-HTTP exporters (metrics to `/v1/metrics`,
traces to `/v1/traces`) with no OpenTelemetry exporter SDK dependency.

```go
package main

import (
    "os"

    "go.putnami.dev/app"
    "go.putnami.dev/telemetry"
)

func main() {
    application := app.New("my-service")
    application.Use(telemetry.NewPlugin(telemetry.Config{
        ServiceName:    "my-service",
        ServiceVersion: "1.0.0",
        OTLP: &telemetry.OTLPConfig{
            Endpoint:    "https://collector.example:4318", // OTLP/HTTP collector base URL
            BearerToken: os.Getenv("OTLP_TOKEN"),          // optional, static
            // For credentials that expire, set BearerTokenSource instead — it
            // resolves the bearer per request and wins over BearerToken. On GCP,
            // telemetry.NewGCPIDTokenSource(collectorURL) authenticates to a
            // private collector with the workload's own identity (no secret).
            // HTTPClient, Headers, FlushInterval (default 10s), Timeout (default 5s) optional
        },
    }))

    application.ListenAndServe()
}
```

`OTLP` only fills `TraceExporter`/`MetricReader` when they are nil, so you can
still supply your own `sdktrace.SpanExporter` / `sdkmetric.Reader` directly via
those fields to override either subsystem. If none of `OTLP`, `TraceExporter`,
or `MetricReader` is set, the providers are created but produce no output.

When the collector requires rotating credentials, provide an `HTTPClient` whose
transport authenticates every request. For example,
`idtoken.NewClient(ctx, collectorURL)` from `google.golang.org/api/idtoken`
returns a client that refreshes Google ID tokens automatically. The injected
client is shared by the metrics, traces, and logs exporters and used verbatim;
the caller owns its timeout, and `OTLPConfig.Timeout` only applies to the
built-in client.

### Configuration

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `ServiceName` | `string` | -- | Sets `service.name` on the OTel resource attached to every span and metric. When unset, telemetry falls back to the SDK default (`unknown_service`). |
| `ServiceVersion` | `string` | -- | Sets `service.version` on the OTel resource attached to every span and metric. |
| `OTLP` | `*telemetry.OTLPConfig` | `nil` | When set, auto-wires the built-in OTLP/JSON push exporters (metrics + traces) to `OTLPConfig.Endpoint`. Only fills `TraceExporter`/`MetricReader` when they are nil. |
| `TraceExporter` | `sdktrace.SpanExporter` | `nil` | Explicit span exporter. Tracing is disabled when nil (and `OTLP` is unset). |
| `MetricReader` | `sdkmetric.Reader` | `nil` | Explicit metric reader. Metrics are disabled when nil (and `OTLP` is unset). |
| `TraceSampleRate` | `*float64` | `telemetry.SampleRate(1.0)` when nil | Sampling ratio (0.0 to 1.0). Set with `telemetry.SampleRate(rate)`; `telemetry.SampleRate(0)` explicitly samples nothing. |
| `ShutdownTimeout` | `time.Duration` | `5s` | Maximum time to wait for exporters to flush on shutdown. |

`TraceSampleRate` is a pointer so the plugin can distinguish an omitted value
from an explicit `0.0`: leave it nil for the default full sampling, or pass
`telemetry.SampleRate(0)` to disable trace sampling.

### DI Integration

During startup the plugin registers two singletons in the DI container:

- `trace.TracerProvider` -- the OpenTelemetry tracer provider
- `metric.MeterProvider` -- the OpenTelemetry meter provider

Any service that depends on these types can inject them directly.

## Tracing

### Creating Spans

Use `StartSpan` to create a span from the global tracer provider. The returned context carries the span for downstream propagation.

```go
ctx, span := telemetry.StartSpan(ctx, "my-service", "handleOrder")
defer span.End()

// ... do work ...
```

For more control, obtain a named tracer and start spans from it:

```go
tracer := telemetry.Tracer("my-service")
ctx, span := tracer.Start(ctx, "processPayment")
defer span.End()
```

### Setting Span Status

Mark a span as successful or failed:

```go
if err != nil {
    telemetry.SetSpanError(span, err)
} else {
    telemetry.SetSpanOK(span)
}
```

`SetSpanError` is a no-op when `err` is nil, so it is safe to call unconditionally.

### Adding Span Attributes

Use `SpanAttrs` for quick string key-value pairs:

```go
telemetry.SpanAttrs(span, "user.id", userID, "request.method", "GET")
```

Key-value pairs must be even in count. An odd count is silently ignored.

### Context Propagation

Retrieve the active span or trace ID from a context:

```go
span := telemetry.SpanFromContext(ctx)

traceID := telemetry.TraceIDFromContext(ctx)
// Returns a 32-character hex string, or "" if no span is active.
```

### Structured Error Recording

When working with `go.putnami.dev/errors` structured errors, use `SetSpanErrorStructured` to record rich error metadata on a span:

```go
telemetry.SetSpanErrorStructured(span, err)
```

This records the error and sets span attributes for `error.code`, `error.category`, `error.retryable`, and `error.source` when available.

## Metrics

### Creating Instruments

Obtain a meter and create instruments:

```go
meter := telemetry.Meter("my-service")

// Counter
requests, _ := telemetry.Counter(meter, "requests_total")
requests.Add(ctx, 1)

// Histogram
duration, _ := telemetry.Histogram(meter, "request_duration_ms")
duration.Record(ctx, 42.5)

// Gauge
temperature, _ := telemetry.Gauge(meter, "temperature")
temperature.Record(ctx, 72.5)

// Up-down counter (values that go up and down, e.g. active connections)
active, _ := telemetry.UpDownCounter(meter, "active_connections")
active.Add(ctx, 1)
active.Add(ctx, -1)
```

All creation helpers accept the same options as their OpenTelemetry counterparts (description, unit, etc.):

```go
counter, _ := telemetry.Counter(meter, "http.requests_total",
    metric.WithDescription("Total HTTP requests served"),
    metric.WithUnit("{request}"),
)
```

## HTTP Middleware

The `HTTPMiddleware` function returns a middleware for `go.putnami.dev/http` that automatically traces every request and records two metrics:

- `http.server.request_count` -- total requests, labeled by method, route, and status code
- `http.server.duration` -- request duration in milliseconds as a histogram

```go
import (
    "go.putnami.dev/telemetry"
    putnami_http "go.putnami.dev/http"
)

server := putnami_http.NewServer()
server.Use(telemetry.HTTPMiddleware("my-service"))
```

Each request produces a span named `{METHOD} {route}` (e.g. `GET /api/users`). Responses with status codes >= 500 are marked as errors on the span.

## Error Bridge

When the telemetry plugin starts, it registers a hook with `go.putnami.dev/errors` that automatically increments an `errors_total` counter every time a structured error is created. The counter is labeled with:

- `error.code` -- the error code (e.g. `"database.connection"`)
- `error.category` -- the error category
- `error.source` -- the source location of the error

This requires no manual instrumentation. Any code that creates errors via the `errors` package automatically contributes to this metric.

## Best Practices

1. **Name tracers and meters by service or package.** This groups telemetry data logically when multiple components share a provider.

2. **Always defer `span.End()`.** Forgetting to end a span causes memory leaks and incomplete traces.

3. **Use `SetSpanErrorStructured` over `SetSpanError`** when working with `go.putnami.dev/errors` to capture structured metadata (code, category, retryable).

4. **Set a sample rate below 1.0 in production.** Full sampling generates significant data volume. Start with `0.1` (10%) and adjust based on traffic.

5. **Use the HTTP middleware early in the middleware chain** so that spans capture the full request lifecycle, including downstream middleware processing time.

6. **Propagate context through function calls.** Spans created with a context that carries a parent span are automatically linked as children, producing a complete trace tree.

7. **Flush on shutdown.** The plugin handles this automatically when used with `app.Module`. If using the providers standalone, call `TracerProvider.Shutdown()` and `MeterProvider.Shutdown()` before exit.

## Contract and compatibility

See the [service telemetry specification](../specs/service-telemetry.json), the
[push-transport ADR](adr/0001-push-otlp-json-without-an-exporter-sdk.md), the
[span-ownership ADR](adr/0002-the-middleware-owns-the-span.md), and [support
evidence](../README.md#support-and-contract). The package is stable and
maintained, with the workspace's [pre-1.0 migration
policy](../../../../RELEASE.md), not strict compatibility between every `0.x`
minor.
