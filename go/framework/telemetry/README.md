# Telemetry

The `telemetry` package provides OpenTelemetry integration for distributed tracing and metrics. It plugs into the application lifecycle as a plugin and registers the tracer/meter providers in the DI container.

## Plugin Setup

```go
import (
    "go.putnami.dev/telemetry"
    sdktrace "go.opentelemetry.io/otel/sdk/trace"
    sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

app.New("my-service").
    Use(telemetry.NewPlugin(telemetry.Config{
        ServiceName:    "my-service",
        ServiceVersion: "1.0.0",
        TraceExporter:  mySpanExporter,   // e.g., OTLP exporter
        MetricReader:   myMetricReader,   // e.g., periodic reader
        TraceSampleRate: telemetry.SampleRate(0.5), // sample 50% of traces
    }))
```

The plugin registers `trace.TracerProvider` and `metric.MeterProvider` in the DI container and shuts them down gracefully on application stop.
Leave `TraceSampleRate` nil to sample every trace. Use `telemetry.SampleRate(0)`
to explicitly sample nothing.

## Default telemetry from the environment

`FromEnvironment` returns the telemetry a service gets without wiring it. Register
it once as a default plugin, and every `app.New` starts with it:

```go
func init() {
    app.RegisterDefaultPlugin(func() app.Plugin {
        return telemetry.FromEnvironment(telemetry.EnvironmentOptions{
            BearerTokenSource: telemetry.NewGCPIDTokenSource, // optional
        })
    })
}
```

It reads the standard OpenTelemetry variables:

| Variable | Effect |
|----------|--------|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | Collector base URL. Without it, the factory returns nil and the application gets no plugin. |
| `OTEL_SDK_DISABLED=true` | Turns the default telemetry off, whatever the endpoint says. |
| `OTEL_SERVICE_NAME`, `OTEL_RESOURCE_ATTRIBUTES` | Service identity on every metric and span. |

The plugin adds `HTTPMiddleware` outermost on every HTTP server of the
application, so a request that an inner middleware refuses is still counted. It
builds the tracer and meter providers that push to the collector, unless the
application installs its own `NewPlugin`: that plugin then owns the providers.
A server that also adds `HTTPMiddleware` itself counts each request once. The
plugin registers nothing in the DI container. See [ADR
0003](doc/adr/0003-telemetry-is-on-by-default-from-the-environment.md).

## Push telemetry (OTLP/JSON over HTTP)

Putnami is serverless-first, so telemetry is **pushed** to a collector rather than scraped. The built-in exporters render the OTLP/JSON wire format (per the [telemetry protocol](../../../protocols/telemetry/README.md)) and POST it — no OpenTelemetry exporter SDK dependency.

Set `Config.OTLP` and the plugin auto-wires the metric reader (`/v1/metrics`) and span exporter (`/v1/traces`):

```go
app.New("my-service").
    Use(telemetry.NewPlugin(telemetry.Config{
        ServiceName:    "my-service",
        ServiceVersion: "1.0.0",
        OTLP: &telemetry.OTLPConfig{
            Endpoint:    "https://collector.example:4318", // OTLP/HTTP collector base URL
            BearerToken: os.Getenv("OTLP_TOKEN"),          // optional
            // HTTPClient, Headers, FlushInterval (default 10s), Timeout (default 5s) optional
        },
    }))
```

`OTLP` only fills `MetricReader`/`TraceExporter` when they are nil, so an explicit reader/exporter still wins. The metric reader flushes periodically and once more on shutdown (driven by `MeterProvider.Shutdown`), so short-lived containers do not drop their last batch. Collector errors are dropped — telemetry never affects the workload.

For collectors that require rotating credentials, set `HTTPClient` to a client
whose transport authenticates each request. For example,
`idtoken.NewClient(ctx, collectorURL)` from `google.golang.org/api/idtoken`
returns a client that refreshes Google ID tokens automatically. An injected
client is used verbatim for metrics, traces, and logs, so its caller owns its
timeout; `OTLPConfig.Timeout` only configures the built-in client.

### Logs

Logs are opt-in via an OTLP log sink you attach to the application logger (exactly like the console/JSON sinks):

```go
sink := telemetry.NewOTLPLogSink(telemetry.OTLPConfig{
    Endpoint:       "https://collector.example:4318",
    ServiceName:    "my-service",
    ServiceVersion: "1.0.0",
})
defer sink.Close() // final flush on shutdown

log := logger.New("my-service", logger.LevelInfo, logger.NewJSONSink(), sink)
```

The sink batches records and POSTs them to `/v1/logs` on a background ticker and on `Close`, so the logging path never blocks on the network.

## Tracing

### Creating Spans

```go
import "go.putnami.dev/telemetry"

// Get a tracer
tracer := telemetry.Tracer("my-service")
ctx, span := tracer.Start(ctx, "handleRequest")
defer span.End()

// Or use the convenience function
ctx, span := telemetry.StartSpan(ctx, "my-service", "handleRequest")
defer span.End()
```

### Span Status and Errors

```go
result, err := doWork(ctx)
if err != nil {
    telemetry.SetSpanError(span, err)
    return err
}
telemetry.SetSpanOK(span)
```

### Span Attributes

```go
telemetry.SpanAttrs(span, "user.id", userID, "request.method", "GET")
```

### Trace ID Extraction

```go
traceID := telemetry.TraceIDFromContext(ctx)
// Returns 32-char hex string, or "" if no active span
```

## Metrics

### Counters

```go
meter := telemetry.Meter("my-service")
counter, _ := telemetry.Counter(meter, "requests_total")
counter.Add(ctx, 1, metric.WithAttributes(
    attribute.String("method", "GET"),
    attribute.String("route", "/users"),
))
```

### Histograms

```go
duration, _ := telemetry.Histogram(meter, "request_duration_ms",
    metric.WithUnit("ms"),
)
duration.Record(ctx, 42.5)
```

### Gauges

```go
temp, _ := telemetry.Gauge(meter, "temperature")
temp.Record(ctx, 72.5)
```

### Up/Down Counters

```go
activeConns, _ := telemetry.UpDownCounter(meter, "active_connections")
activeConns.Add(ctx, 1)  // connection opened
activeConns.Add(ctx, -1) // connection closed
```

## HTTP Middleware

Automatically trace HTTP requests and record metrics:

```go
import "go.putnami.dev/telemetry"

server := http.NewServerPlugin(config).
    Use(telemetry.HTTPMiddleware("my-service"))
```

The middleware creates a span per request with attributes (`http.method`, `http.route`, `http.status_code`) and records `http.server.request_count` and `http.server.duration` metrics.

The span is always ended, even if a downstream handler or middleware panics: the panic is recovered just long enough to mark the span errored (`http.panic=true`, `http.status_code=500`) and record metrics, then re-panicked so any outer recovery middleware behaves normally. This means `HTTPMiddleware` can safely sit outermost regardless of where a recovery middleware is installed.

When two `HTTPMiddleware` wrap the same request, for example the one from
`FromEnvironment` and one the workload adds, only the outer one records it.

## Configuration

| Field | Default | Description |
|-------|---------|-------------|
| `ServiceName` | `""` | Name in traces and metrics |
| `ServiceVersion` | `""` | Version in traces and metrics |
| `TraceExporter` | `nil` | Span exporter (nil = tracing disabled) |
| `MetricReader` | `nil` | Metric reader (nil = metrics disabled) |
| `OTLP` | `nil` | When set, auto-wires the built-in OTLP/JSON push exporters (metrics + traces). See [Push telemetry](#push-telemetry-otlpjson-over-http). |
| `TraceSampleRate` | `1.0` when nil | Sampling ratio (0.0-1.0). Set with `telemetry.SampleRate(rate)`; `SampleRate(0)` disables trace sampling. |
| `ShutdownTimeout` | `5s` | Max time to flush on shutdown |

## Support and contract

The SDD owner is `go`. `go.putnami.dev/telemetry` is public, documented, maintained,
and classified `stable` in the workspace [support
catalog](../../../putnami.support.json). Before v1.0.0, a minor `0.x` release may
still contain a breaking change; the [release policy](../../../RELEASE.md) requires
release notes and migration documentation rather than strict compatibility between
every pre-1.0 minor.

The [service telemetry specification](specs/service-telemetry.json) defines what
the framework adds, backed by two durable decisions: [push OTLP/JSON without an
exporter SDK](doc/adr/0001-push-otlp-json-without-an-exporter-sdk.md), [the
middleware owns the span](doc/adr/0002-the-middleware-owns-the-span.md), and
[telemetry is on by default from the
environment](doc/adr/0003-telemetry-is-on-by-default-from-the-environment.md). The
export defaults, the final flush and the dropped collector error belong to the
feature of the [telemetry protocol](../../../protocols/telemetry/README.md),
`telemetry/otlp-push-contract`; the exporter, log sink and mapping tests here
prove its checks.

Regression evidence covers [tracing, metrics, HTTP middleware, sampling, and the
error bridge](telemetry_test.go), [plugin lifecycle purity and bounded
shutdown](plugin_extra_test.go), and [the OTLP/JSON exporters, log sink, and
mapping tables](otlp_test.go), and [the default telemetry from the
environment](environment_test.go).
