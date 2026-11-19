# Add observability

You will push your app's metrics and logs to an OpenTelemetry collector as OTLP/JSON — no scraping, no vendor SDK.

Putnami is serverless-first: instances scale to zero and are never externally addressable, so a `/metrics` scrape can't reach them. Telemetry is **pushed** instead, as standard OTLP/JSON over HTTP, identically from the TypeScript and Go runtimes.

## Steps

### 1) Enable the telemetry plugin

On the TypeScript runtime, setting `OTEL_EXPORTER_OTLP_ENDPOINT` to your collector's base URL is enough: every `application()` then pushes to it, and `OTEL_SDK_DISABLED=true` turns it off. Putnami Cloud sets the variable for you. On both runtimes, telemetry from the environment names its process in `service.instance.id`, unless `OTEL_RESOURCE_ATTRIBUTES` sets one, so each instance of a scaled-out service writes its own metric series. To configure telemetry in code instead, register the `telemetry` plugin and point it at your collector's base URL:

```typescript
import { telemetry } from '@putnami/application';

app.use(
  telemetry({
    enabled: true,
    endpoint: 'https://collector.example:4318', // OTLP/HTTP collector base URL
  }),
);
```

With the plugin active, framework metrics (HTTP, SQL pool, storage, …) are collected and POSTed to `<endpoint>/v1/metrics` automatically — no per-metric wiring.

### 2) Configure it from the environment

`telemetry` is a config section, so the endpoint and token stay out of code and vary per environment:

```yaml
# .env.production.yaml
telemetry:
  enabled: true
  endpoint: "${TELEMETRY_ENDPOINT}"
  bearer: "${OTLP_TOKEN}" # sensitive — kept in the environment, never in the file
  metricIntervalS: 60
```

Config string values interpolate `${VAR}` references from the environment when
the config is loaded, so the endpoint and the sensitive `bearer` token come from
the deployment environment and never land in the file. Use `${VAR:-fallback}` to
supply a default when the variable is unset.

Traces follow a budget: each instance keeps its first 100 root traces, then
1 % of them, with at least 1 root each minute. Set `telemetry.traces.budget`,
`ratio` and `floorPerMinute` to change it, in Go and in TypeScript. A change
to a file under `conf/` applies within 30 seconds, without a restart; a change
in `CONFIG_DATA` or Putnami Cloud config applies at the next start.

The collector can tighten every service from its export responses, without a
redeploy: a `Putnami-Export-Directive: trace-ratio=0.001, log-level=warn,
ttl=300` header caps the trace ratio and floor and raises the log level for
`ttl` seconds, at most an hour, and errors are always emitted. A `429` with a
`Retry-After` delays the next export of that signal.

Telemetry is opt-in and has **no default endpoint**: if it is enabled but no endpoint is set, the plugin logs a warning and stays off, so data is never shipped to a hardcoded collector.

### 3) Emit your own metrics

Record app-specific metrics from anywhere in your code — they ride the same push:

```typescript
import { incCounter, setGauge, observeHistogram } from '@putnami/application';

incCounter('orders.placed');
setGauge('queue.depth', queue.length);
observeHistogram('checkout.duration_ms', elapsed);
```

### 4) The same from Go

The Go runtime speaks the identical wire format. Add the telemetry plugin and set `OTLP`:

```go
import "go.putnami.dev/telemetry"

app.New("checkout").
    Use(telemetry.NewPlugin(telemetry.Config{
        ServiceName:    "checkout",
        ServiceVersion: "1.0.0",
        OTLP: &telemetry.OTLPConfig{
            Endpoint:    "https://collector.example:4318",
            BearerToken: os.Getenv("OTLP_TOKEN"),
        },
    }))
```

Setting `OTLP` auto-wires the metric reader (`/v1/metrics`) and the trace span exporter (`/v1/traces`). The metric reader flushes periodically and once more on shutdown, so short-lived containers never drop their last batch.

### 5) Ship logs too

On the Go runtime, attach an OTLP log sink to your application logger and records are batched and POSTed to `<endpoint>/v1/logs`.

```go
// Go
sink := telemetry.NewOTLPLogSink(telemetry.OTLPConfig{
    Endpoint:    "https://collector.example:4318",
    ServiceName: "checkout",
})
defer sink.Close() // final flush on shutdown
log := logger.New("checkout", logger.LevelInfo, logger.NewJSONSink(), sink)
```

On the TypeScript runtime, `application()` attaches the equivalent sink, `OtlpLogSink` from `@putnami/application`, to the root logger by itself when `OTEL_EXPORTER_OTLP_ENDPOINT` is set, with the same resource and token source as metrics and traces. To push logs elsewhere, add your own `OtlpLogSink` to the root logger with `useLogger().addSink(sink)` before the application starts; `application()` then adds none.

## HTTP server metrics

Both runtimes record the HTTP requests a service answers under the same two metrics, with the same unit, labels and buckets:

| Metric | Type | Unit | Labels |
|---|---|---|---|
| `http.server.request_count` | Counter | — | `http.method`, `http.route`, `http.status_code` |
| `http.server.duration` | Histogram | `ms` | `http.method`, `http.route`, `http.status_code` |

- `http.route` is the route pattern (`/users/[id]` in TypeScript, `/users/{id}` in Go), never the request path, so series stay bounded. A TypeScript request that matches no route is labeled `__unmatched__`, and a nonstandard method `_OTHER`; the Go runtime answers such a request before its middleware runs and does not record it.
- `http.status_code` is the response status, an integer. Error rates filter on it: there is no separate error counter.
- `http.server.duration` counts each request in the buckets bounded by 0, 5, 10, 25, 50, 75, 100, 250, 500, 750, 1000, 2500, 5000, 7500 and 10000 ms, so a backend computes percentiles from it.
- The TypeScript runtime pushes per-second deltas, and the Go runtime cumulative totals. A backend that stores cumulative series, such as Prometheus, needs the collector's `deltatocumulative` processor in front of it for TypeScript services.

With the default OTLP-to-Prometheus name translation, which adds the unit and the counter suffix, the request rate is `rate(http_server_request_count_total[5m])` and the p95 latency `histogram_quantile(0.95, sum by (le) (rate(http_server_duration_milliseconds_bucket[5m])))`. A collector configured without suffixes drops `_total` and `_milliseconds`.

The [telemetry protocol](https://github.com/putnami/putnami/tree/main/protocols/telemetry) commits these names, labels and bounds, and both runtimes are tested against them.

## What you get

- **Metrics** — framework metrics out of the box, plus your own counters, gauges, and histograms.
- **Traces** — request spans from both runtimes, and one consumer span per event delivery, `process <topic>`, that continues the trace of the span that published the event. A request and the work its events start share one trace, even when a Go service publishes and a TypeScript service consumes.
- **Logs** — structured logs forwarded to the collector through the OTLP log sink, attached from the environment on the TypeScript runtime.

Any standard OTLP/HTTP collector accepts this data. Collector errors are dropped by design — telemetry never affects the workload's request path.
