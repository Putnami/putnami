# Add observability

You will push your app's metrics and logs to an OpenTelemetry collector as OTLP/JSON — no scraping, no vendor SDK.

Putnami is serverless-first: instances scale to zero and are never externally addressable, so a `/metrics` scrape can't reach them. Telemetry is **pushed** instead, as standard OTLP/JSON over HTTP, identically from the TypeScript and Go runtimes.

## Steps

### 1) Enable the telemetry plugin

Register the `telemetry` plugin on your application and point it at your collector's base URL:

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
  flushIntervalS: 30
```

Config string values interpolate `${VAR}` references from the environment when
the config is loaded, so the endpoint and the sensitive `bearer` token come from
the deployment environment and never land in the file. Use `${VAR:-fallback}` to
supply a default when the variable is unset.

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

Logs are opt-in: attach an OTLP log sink to your application logger and records are batched and POSTed to `<endpoint>/v1/logs`.

```go
// Go
sink := telemetry.NewOTLPLogSink(telemetry.OTLPConfig{
    Endpoint:    "https://collector.example:4318",
    ServiceName: "checkout",
})
defer sink.Close() // final flush on shutdown
log := logger.New("checkout", logger.LevelInfo, logger.NewJSONSink(), sink)
```

On the TypeScript runtime the equivalent sink is `OtlpLogSink` from `@putnami/application`, added to the application logger the same way as the console/JSON sinks.

## What you get

- **Metrics** — framework metrics out of the box, plus your own counters, gauges, and histograms.
- **Traces** — request spans on the Go runtime (`telemetry.Tracer(...)`).
- **Logs** — structured logs forwarded to the collector through the opt-in OTLP sink.

Any standard OTLP/HTTP collector accepts this data. Collector errors are dropped by design — telemetry never affects the workload's request path.
