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
        TraceSampling:  &telemetry.TraceSampling{Budget: 100, Ratio: 0.5, FloorPerMinute: 1},
    }))
```

The plugin registers `trace.TracerProvider` and `metric.MeterProvider` in the DI container and shuts them down gracefully on application stop.
Leave `TraceSampling` nil for the default trace budget. A zero
`telemetry.TraceSampling{}` samples no root trace.

## Trace budget

Each instance keeps its first `budget` root traces, then keeps a root when
`ratio` picks its trace ID, and keeps at least `floorPerMinute` roots each UTC
minute, so a rarely called service still exports traces. A span with a parent,
remote or local, follows its parent's sampled flag: a service follows its
caller's decision.

Behind a load balancer such as Cloud Run's, the edge sends a `traceparent`
whose sampled flag is usually off, so following it drops almost every server
span. With `parent: decide`, a server span whose parent came from outside, a
`traceparent` without an `x-putnami-traceparent` on the same trace, takes a
root decision by the budget instead, and keeps the parent's trace ID. A call
from another Putnami service sends both headers, so its decision still holds.

| Key in `telemetry.traces` | Default | Meaning |
|---|---|---|
| `budget` | `100` | Root traces the instance keeps first |
| `ratio` | `0.01` | Share of root traces kept once the budget is spent, from 0 to 1 |
| `floorPerMinute` | `1` | Root traces kept each minute whatever the ratio picks |
| `parent` | `follow` | `follow` an outside parent's sampled flag, or `decide` as a root |

```yaml
# conf/.env.yaml, or CONFIG_DATA
telemetry:
  traces:
    budget: 100
    ratio: 0.01
    floorPerMinute: 1
    parent: decide
```

`Config.TraceSampling` sets the same values in code (`Parent` takes
`telemetry.ParentFollow` or `telemetry.ParentDecide`); each key of the
config section overrides its field. While it runs, the plugin reloads the
config sources every 30 seconds and applies a changed value without a restart.
A change to a file under `conf/` applies within 30 seconds. `CONFIG_DATA` keeps
its first load for the life of the process, and so do the Putnami Cloud remote
sources unless `CONFIG_SERVER_REFRESH` sets a refresh period: a Cloud config
change then applies within that period plus a minute. To change a running
process otherwise, call `plugin.Sampler().Update`.
A value out of range keeps the settings in force and logs a warning.

The counter `trace.decisions`, labeled `outcome` `kept` or `dropped`, counts
each root decision. `Plugin.Sampler()` returns the sampler: `Settings()` reads
its own settings, without an export directive's, and `Update(settings)` replaces them at runtime until
the config section changes. The TypeScript runtime applies the same keys, rule
and counter, which `go.putnami.dev/protocol/telemetry` defines.

### Export directives

An export response may carry `Putnami-Export-Directive: trace-ratio=1,
trace-parent=decide, log-level=warn, ttl=300`. For `ttl` seconds, at most an
hour, each trace setting the directive carries (`trace-budget`, `trace-ratio`,
`trace-floor`, `trace-parent`) replaces the service's own, higher or lower;
the service applies the higher log level of the directive and its own, and
still emits every error. Putnami Cloud sends the settings a person set with
`putnami cloud telemetry set`. A `429` with a `Retry-After` delays the next export of that signal only.
See [Telemetry](../../doc/framework/13-telemetry.md#export-directives).

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

Every process also names its own instance: the resource carries a random
`service.instance.id`, drawn once per process, unless `OTEL_RESOURCE_ATTRIBUTES`
sets one. Two instances of one revision then write two metric series, which a
collector that requires points in order accepts.

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
            // HTTPClient, Headers, FlushInterval (logs, default 10s), MetricInterval (default 60s), Timeout (default 5s) optional
        },
    }))
```

`OTLP` only fills `MetricReader`/`TraceExporter` when they are nil, so an explicit reader/exporter still wins. The metric reader exports every 60 seconds, and the span batcher exports at most 30 seconds apart, or at once when 512 spans wait. `OTLPConfig.MetricInterval` and `Config.SpanBatchDelay` override those defaults; when they are zero, `OTEL_METRIC_EXPORT_INTERVAL` and `OTEL_BSP_SCHEDULE_DELAY`, in milliseconds, do. The metric reader flushes once more on shutdown (driven by `MeterProvider.Shutdown`), so short-lived containers do not drop their last batch. Collector errors are dropped — telemetry never affects the workload.

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

A proxy between two services may insert a span it never exports and rewrite
the parent ID of `traceparent`. A generated client therefore writes its
`traceparent` value twice, the second time in `x-putnami-traceparent`, which
proxies leave alone. When both headers carry the same trace ID and different
parent IDs, the server span parents to the one in `x-putnami-traceparent`, so
the trace keeps the call as one edge. When the second header is absent,
malformed or on another trace, the server uses `traceparent`. The plugin
installs this as `telemetry.ProxySafeTraceContext`; the TypeScript runtime
applies the same rule.

## Configuration

| Field | Default | Description |
|-------|---------|-------------|
| `ServiceName` | `""` | Name in traces and metrics |
| `ServiceVersion` | `""` | Version in traces and metrics |
| `TraceExporter` | `nil` | Span exporter (nil = tracing disabled) |
| `MetricReader` | `nil` | Metric reader (nil = metrics disabled) |
| `OTLP` | `nil` | When set, auto-wires the built-in OTLP/JSON push exporters (metrics + traces). See [Push telemetry](#push-telemetry-otlpjson-over-http). |
| `TraceSampling` | `DefaultTraceSampling()` when nil: budget 100, ratio 0.01, floor 1 per minute | Trace budget; each `telemetry.traces` config key overrides its field; a `conf/` file change applies within 30 s. A zero value samples no root. |
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
