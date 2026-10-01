# Telemetry

The `TelemetryPlugin` collects application metrics and pushes them to a standard **OpenTelemetry collector** as **OTLP/JSON over HTTP**. It auto-instruments HTTP requests and provides a simple API for custom counters, gauges, and histograms.

Putnami is serverless-first, so telemetry is *pushed* (not scraped): metrics are aggregated per second in memory and POSTed to the collector's `/v1/metrics` every N seconds. The wire format is the canonical OTLP/JSON envelope shared with the Go runtime — see the [telemetry protocol](../../../../protocols/telemetry/README.md) — so the same collector ingests both runtimes identically.

> **Telemetry is opt-in.** It is **disabled by default** and has **no default endpoint**. No operational data ever leaves the process unless you explicitly set `enabled: true` **and** configure an `endpoint`. If telemetry is enabled without an endpoint, the plugin logs a warning and stays inert — it never falls back to a hardcoded destination.

## Quick Start

```typescript
import { application, http, telemetry } from '@putnami/application';

const app = application()
  .use(
    telemetry({
      enabled: true,
      endpoint: process.env.OTLP_ENDPOINT, // collector base URL, e.g. https://collector:4318
      bearer: process.env.OTLP_TOKEN,
    }),
  )
  .use(http({ port: 3000 }));

await app.start();
// HTTP metrics are collected automatically.
```

## How It Works

1. **Collect** — Every HTTP request records per-route counters and duration histograms via a middleware. Custom metrics are recorded with `incCounter()`, `setGauge()`, and `observeHistogram()`.
2. **Aggregate** — Metrics are bucketed by Unix second in memory. This keeps the memory footprint small and gives the backend per-second resolution.
3. **Flush** — A periodic timer drains completed buckets, renders them as an OTLP/JSON metrics request, and POSTs it to `<endpoint>/v1/metrics`. On shutdown, remaining buckets are flushed immediately. Collector errors are dropped — telemetry never affects application behaviour.

## Configuration

```typescript
telemetry({
  enabled: true,                  // default: false (opt-in)
  endpoint: 'https://collector:4318', // OTLP/HTTP collector base URL — required when enabled, no default
  bearer: 'my-token',
  flushIntervalS: 30,             // default (seconds)
  app: 'my-service',              // defaults to package name; reported as service.name
});
```

| Option | Type | Default | Description |
|---|---|---|---|
| `enabled` | `boolean` | `false` | Enable telemetry collection (explicit opt-in) |
| `endpoint` | `string` | — (required when enabled) | OTLP/HTTP collector **base URL**; metrics POST to `<endpoint>/v1/metrics`. No default, so data is never shipped to a hardcoded destination |
| `bearer` | `string` | — | Bearer token for the collector |
| `flushIntervalS` | `number` | `30` | Flush interval in seconds |
| `app` | `string` | — | Reported as the OTLP resource's `service.name`; defaults to package name |

If `enabled` is `true` but no `endpoint` is set, the plugin logs a warning and does not start. The endpoint is any standard OTLP/HTTP collector base URL.

Configuration can also be set via YAML:

```yaml
# conf/.env.production.yaml
telemetry:
  enabled: true
  endpoint: "https://collector:4318"
  bearer: "..."
  flushIntervalS: 30
  app: "my-service"
```

## Auto-Collected HTTP Metrics

The plugin registers a middleware that records the following for every request:

| Metric | Type | Name Pattern | Example |
|---|---|---|---|
| Request count | Counter | `http.{METHOD}.{route}.{status}` | `http.GET./api/users.200` |
| Response duration | Histogram | `http.{METHOD}.{route}.duration` | `http.GET./api/users.duration` |
| Client errors | Counter | `http.error.4xx` | — |
| Server errors | Counter | `http.error.5xx` | — |

Routes use the **pattern** (`/users/[id]`), not the resolved path (`/users/123`). This keeps cardinality bounded.

## Custom Metrics

Three metric types are available as standalone functions. They are safe to call anywhere — if telemetry is disabled they are no-ops.

### Counter

A monotonically increasing value.

```typescript
import { incCounter } from '@putnami/application';

incCounter('app.orders.created');
incCounter('app.emails.sent', 3);
```

### Gauge

A point-in-time value that can go up or down.

```typescript
import { setGauge } from '@putnami/application';

setGauge('app.queue.size', pendingJobs.length);
setGauge('app.connections.active', pool.active);
```

### Histogram

A distribution of observed values (tracks count, sum, min, max per second).

```typescript
import { observeHistogram } from '@putnami/application';

const start = Date.now();
await processOrder(order);
observeHistogram('app.order.processing.duration', Date.now() - start);
```

## Wire Format

Each flush POSTs an OTLP/JSON `ExportMetricsServiceRequest` to `<endpoint>/v1/metrics`. Counters become monotonic delta `Sum`s, gauges become `Gauge`s, and histograms become `Histogram`s (count/sum/min/max). The resource carries `service.name`, optional `service.version`, and `putnami.framework: typescript`.

```json
{
  "resourceMetrics": [
    {
      "resource": {
        "attributes": [
          { "key": "putnami.framework", "value": { "stringValue": "typescript" } },
          { "key": "service.name", "value": { "stringValue": "my-service" } }
        ]
      },
      "scopeMetrics": [
        {
          "scope": { "name": "putnami" },
          "metrics": [
            {
              "name": "http.GET./api/users.200",
              "sum": {
                "dataPoints": [{ "startTimeUnixNano": "1738800000000000000", "timeUnixNano": "1738800001000000000", "asInt": "42" }],
                "aggregationTemporality": 1,
                "isMonotonic": true
              }
            }
          ]
        }
      ]
    }
  ]
}
```

This is the canonical envelope defined by the [telemetry protocol](../../../../protocols/telemetry/README.md), byte-identical to what the Go runtime emits for equivalent input.

## Logs

Logs are pushed with the same wire format via an opt-in `OtlpLogSink` you attach to the application logger:

```typescript
import { Logger, JsonSink } from '@putnami/runtime';
import { OtlpLogSink } from '@putnami/application';

const otlp = new OtlpLogSink({ endpoint: 'https://collector:4318', serviceName: 'my-service' });
const logger = new Logger([new JsonSink(), otlp], 'my-service');
// on shutdown:
await otlp.close(); // flushes remaining records to /v1/logs
```

The sink batches records and POSTs them to `<endpoint>/v1/logs` on a background ticker (10s default) and on `close()`, so the logging path never blocks on the network. A bounded queue drops the oldest records if the collector wedges.

## Lifecycle

| Phase | Behavior |
|---|---|
| `warmup` | Initializes the collector, registers HTTP middleware |
| `start` | Starts the periodic flush timer |
| `stop` | Drains all remaining buckets (including the current second), POSTs a final OTLP/JSON request, clears the timer |

The flush timer is `unref()`'d so it does not keep the process alive. All telemetry failures are silently dropped — they never affect application behavior.

## Best Practices

1. **Use dot-delimited metric names** — e.g. `app.module.metric` for consistent namespacing.
2. **Keep counter cardinality bounded** — Avoid dynamic segments in metric names. Use route patterns, not resolved paths.
3. **Use histograms for durations** — They give you count, sum, min, and max in a single metric.
4. **Set the `app` option** — Makes it easy to filter metrics per service in the backend.
5. **Configure bearer via YAML/env** — Never hardcode tokens in source code.

## Troubleshooting

### Metrics not appearing in the backend

- Check that `enabled` is `true` — telemetry is **disabled by default**.
- Check that `endpoint` is set — there is **no default endpoint**. If it is missing, the logs show `telemetry is enabled but no endpoint is configured`.
- Verify the `bearer` token is valid for the target environment.
- Check application logs for `telemetry flush every Ns` at startup — if missing, the plugin did not initialize.

### High memory usage

The collector only keeps the current second's bucket plus any unflushed completed buckets. If the flush fails repeatedly, completed buckets accumulate. Ensure the endpoint is reachable.

### Metric names contain resolved paths instead of patterns

The middleware uses `ctx.route` (the matched route pattern). If a request does not match any route, the raw path is used as a fallback. Register routes before the telemetry middleware to ensure patterns are available.
