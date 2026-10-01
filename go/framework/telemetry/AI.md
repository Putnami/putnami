# go.putnami.dev/telemetry

OpenTelemetry integration with distributed tracing, metrics, and HTTP middleware.

## Quick Start

```go
import (
	"go.putnami.dev/app"
	"go.putnami.dev/telemetry"
)

// Supply a TraceExporter and/or MetricReader to enable the respective subsystem.
plugin := telemetry.NewPlugin(telemetry.Config{
	ServiceName:    "my-service",
	ServiceVersion: "1.0.0",
	TraceExporter:  traceExporter, // sdktrace.SpanExporter; nil disables tracing
	MetricReader:   metricReader,  // sdkmetric.Reader; nil disables metrics
	TraceSampleRate: telemetry.SampleRate(0.5), // optional; nil defaults to 1.0
})

a := app.New("my-service")
a.Use(plugin)
a.ListenAndServe()
```

## Tracing

```go
ctx, span := telemetry.StartSpan(ctx, "my-service", "process-order")
defer span.End()

telemetry.SpanAttrs(span, "order.id", orderID) // string key/value pairs
span.AddEvent("payment processed")

if err != nil {
	telemetry.SetSpanError(span, err)
} else {
	telemetry.SetSpanOK(span)
}
```

## Metrics

```go
meter := telemetry.Meter("my-service")

counter, _ := telemetry.Counter(meter, "orders_processed")
counter.Add(ctx, 1)

histogram, _ := telemetry.Histogram(meter, "request_duration_ms")
histogram.Record(ctx, 42.5)
```

`Gauge` and `UpDownCounter` are also available. Creation helpers accept the same
options as their OpenTelemetry counterparts (e.g. `metric.WithDescription`,
`metric.WithUnit`).

## HTTP Middleware

```go
server.Use(telemetry.HTTPMiddleware("my-service")) // auto-traces all requests
```

See `doc/getting-started.md` for full reference.

## Maintained contract

This public package is `stable` in the workspace [support
catalog](../../../putnami.support.json). Its behavior is defined by the [service
telemetry specification](specs/service-telemetry.json), the [push-transport
ADR](doc/adr/0001-push-otlp-json-without-an-exporter-sdk.md), and the
[span-ownership ADR](doc/adr/0002-the-middleware-owns-the-span.md). Before v1.0,
follow the workspace [migration-based compatibility
policy](../../../RELEASE.md); do not infer strict compatibility between every
`0.x` minor.
