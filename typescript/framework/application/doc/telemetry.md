# Telemetry

The `TelemetryPlugin` collects application metrics and pushes them to a standard **OpenTelemetry collector** as **OTLP/JSON over HTTP**. It auto-instruments HTTP requests and provides a simple API for custom counters, gauges, and histograms.

Putnami is serverless-first, so telemetry is *pushed* (not scraped): metrics are aggregated per second in memory and POSTed to the collector's `/v1/metrics` every N seconds. The wire format is the canonical OTLP/JSON envelope shared with the Go runtime — see the [telemetry protocol](../../../../protocols/telemetry/README.md) — so the same collector ingests both runtimes identically.

## From the environment

When the environment sets `OTEL_EXPORTER_OTLP_ENDPOINT`, every `application()` adds the telemetry plugin by itself and pushes to that collector ([ADR 0007](./adr/0007-telemetry-is-on-by-default-from-the-environment.md)). Putnami Cloud sets this variable on every workload.

| Variable | Effect |
|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | Collector base URL. Without it, the application gets no telemetry and nothing leaves the process. |
| `OTEL_SDK_DISABLED=true` | Turns this default telemetry off, whatever the endpoint says. |
| `OTEL_SERVICE_NAME` | The `service.name` of every pushed resource; without it, the `service.name` pair of `OTEL_RESOURCE_ATTRIBUTES`, then the plugin's `app` option or the log sink's `serviceName` option. |
| `OTEL_RESOURCE_ATTRIBUTES` | Comma-separated `key=value` pairs added to every pushed resource. `putnami.framework` keeps the framework's value. |
| `OTEL_METRIC_EXPORT_INTERVAL` | Milliseconds between two metric pushes when the plugin sets no interval. Default 60000. |
| `OTEL_BSP_SCHEDULE_DELAY` | Longest wait, in milliseconds, of a completed span before its batch is pushed, when the plugin sets no delay. Default 30000. |
| `OTEL_BSP_MAX_EXPORT_BATCH_SIZE` | Spans that make a full batch, pushed at once. Default 512. |

Every pushed resource also names its process: a random `service.instance.id`, drawn once per process, unless `OTEL_RESOURCE_ATTRIBUTES` sets one. Two instances of one revision then write two metric series, which a collector that requires points in order accepts.

An application that registers `telemetry()` itself owns its metrics and traces: the environment adds no second plugin, so a request is counted once.

The same variables export logs: `application()` attaches an `OtlpLogSink` to the root logger when it starts, unless that logger already holds one the application configured, and flushes it once a `run()` runner returns. Applications started in the same process share that sink; the last one to stop detaches and closes it. An explicit `telemetry()` plugin exports no logs, so it does not stop this sink; `OTEL_SDK_DISABLED=true` does.

## Explicit plugin

> **The explicit plugin is opt-in.** It is **disabled by default** and has **no default endpoint**. If it is enabled without an endpoint, it logs a warning and stays inert — it never falls back to a hardcoded destination.

```typescript
import { application, http, telemetry } from '@putnami/application';

const app = application()
  .use(
    telemetry({
      enabled: true,
      endpoint: process.env.OTLP_ENDPOINT, // collector base URL, e.g. https://collector:4318
    }),
  )
  .use(http({ port: 3000 }));

await app.start();
// HTTP metrics are collected automatically.
```

## Credentials

Before each push, the plugin asks the telemetry token source registered with `registerTelemetryTokenSource` from `@putnami/runtime` for a bearer, so an expiring token is renewed without a restart. `register()` from `@putnami/cloud/runtime` registers a source that returns a Google ID token for the collector on Google Cloud. A static `bearer` wins over the source. When the source fails, the batch is dropped. The first collector refusal (`401` or `403`) and the first credential failure each log one warning.

## How It Works

1. **Collect** — Every HTTP request is counted in `http.server.request_count` and timed in `http.server.duration`, labeled by method, route pattern and status code, and records a span, via a middleware that runs in front of every other one on each HTTP server the application composes, so a refused request is counted too. Telemetry starts no server of its own. Custom metrics are recorded with `incCounter()`, `setGauge()`, and `observeHistogram()`.
2. **Aggregate** — Metrics are bucketed by Unix second in memory. This keeps the memory footprint small and gives the backend per-second resolution.
3. **Flush** — Every 60 seconds a timer drains completed buckets, renders them as an OTLP/JSON metrics request, and POSTs it to `<endpoint>/v1/metrics`. Completed spans go to `<endpoint>/v1/traces` in batches at most 30 seconds apart, and a batch of 512 spans leaves at once. On shutdown, remaining buckets and spans are flushed immediately. Collector errors are dropped — telemetry never affects application behaviour.

## Configuration

```typescript
telemetry({
  enabled: true,                  // default: false (opt-in)
  endpoint: 'https://collector:4318', // OTLP/HTTP collector base URL — required when enabled, no default
  bearer: 'my-token',
  metricIntervalS: 60,            // default (seconds)
  spanBatchDelayS: 30,            // default (seconds)
  app: 'my-service',              // service.name when OTEL_SERVICE_NAME is unset
});
```

| Option | Type | Default | Description |
|---|---|---|---|
| `enabled` | `boolean` | `false` | Enable telemetry collection (explicit opt-in) |
| `endpoint` | `string` | — (required when enabled) | OTLP/HTTP collector **base URL**; metrics POST to `<endpoint>/v1/metrics`. No default, so data is never shipped to a hardcoded destination |
| `bearer` | `string` | — | Static bearer token; wins over the registered token source |
| `metricIntervalS` | `number` | `60` | Seconds between two metric pushes. When unset, `OTEL_METRIC_EXPORT_INTERVAL` (milliseconds) sets it |
| `spanBatchDelayS` | `number` | `30` | Longest wait, in seconds, of a completed span before its batch is pushed; 512 spans push at once. When unset, `OTEL_BSP_SCHEDULE_DELAY` (milliseconds) sets it |
| `app` | `string` | — | The OTLP resource's `service.name` when neither `OTEL_SERVICE_NAME` nor a `service.name` resource attribute is set; `unknown` without all three |

`metricIntervalS` and `spanBatchDelayS` replace `flushIntervalS`, which pushed metrics and spans together every 30 seconds. The configuration ignores a `flushIntervalS` key: rename it to `metricIntervalS`.

If `enabled` is `true` but no `endpoint` is set, the plugin logs a warning and does not start. The endpoint is any standard OTLP/HTTP collector base URL.

Configuration can also be set via YAML:

```yaml
# conf/.env.production.yaml
telemetry:
  enabled: true
  endpoint: "https://collector:4318"
  bearer: "..."
  metricIntervalS: 60
  app: "my-service"
```

## Auto-Collected HTTP Metrics

The plugin registers a middleware that records the following for every request:

`http.server.request_count` and `http.server.duration`, labeled by method, route pattern and status code, the same as the Go runtime. [Add observability](/docs/how-to/add-observability#http-server-metrics) lists them, with their unit and histogram buckets.

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

A distribution of observed values: count, sum, min, max and a count per bucket, bounded at 0, 5, 10, 25, 50, 75, 100, 250, 500, 750, 1000, 2500, 5000, 7500 and 10000, per second.

```typescript
import { observeHistogram } from '@putnami/application';

const start = Date.now();
await processOrder(order);
observeHistogram('app.order.processing.duration', Date.now() - start);
```

## Wire Format

Each flush POSTs an OTLP/JSON `ExportMetricsServiceRequest` to `<endpoint>/v1/metrics`. Counters become monotonic delta `Sum`s, gauges become `Gauge`s, and histograms become `Histogram`s with count, sum, min, max, their explicit bucket bounds and a count per bucket. A metric with a unit, such as `http.server.duration` in `ms`, carries it. The resource carries `service.name`, optional `service.version`, `putnami.framework: typescript`, and the `service.instance.id` of the process.

```json
{
  "resourceMetrics": [
    {
      "resource": {
        "attributes": [
          { "key": "putnami.framework", "value": { "stringValue": "typescript" } },
          { "key": "service.instance.id", "value": { "stringValue": "5f0c6c2e-8d1a-4f3b-9a7e-2b4d6e8f0a1c" } },
          { "key": "service.name", "value": { "stringValue": "my-service" } }
        ]
      },
      "scopeMetrics": [
        {
          "scope": { "name": "putnami" },
          "metrics": [
            {
              "name": "http.server.request_count",
              "description": "Total HTTP requests",
              "sum": {
                "dataPoints": [
                  {
                    "attributes": [
                      { "key": "http.method", "value": { "stringValue": "GET" } },
                      { "key": "http.route", "value": { "stringValue": "/api/users" } },
                      { "key": "http.status_code", "value": { "intValue": "200" } }
                    ],
                    "startTimeUnixNano": "1738800000000000000",
                    "timeUnixNano": "1738800001000000000",
                    "asInt": "42"
                  }
                ],
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

Logs are pushed with the same wire format by an `OtlpLogSink`. When the environment names a collector, `application()` attaches one to the root logger (see [From the environment](#from-the-environment)). To push logs to another collector, add your own sink to the root logger before `start()`; `application()` then attaches none:

```typescript
import { useLogger } from '@putnami/runtime';
import { OtlpLogSink } from '@putnami/application';

const otlp = new OtlpLogSink({ endpoint: 'https://collector:4318', serviceName: 'my-service' });
const remove = useLogger().addSink(otlp);
// on shutdown:
remove();
await otlp.close(); // flushes remaining records to /v1/logs
```

A sink on a logger you build yourself (`new Logger([...])`) receives only that logger's records, and does not stop `application()` from attaching the environment's sink to the root logger.

The sink batches records and POSTs them to `<endpoint>/v1/logs` on a background ticker (10s default) and on `close()`, so the logging path never blocks on the network. A bounded queue drops the oldest records if the collector wedges.

Its resource carries the same service identity as metrics and traces: `OTEL_SERVICE_NAME`, then the `service.name` pair of `OTEL_RESOURCE_ATTRIBUTES`, then its `serviceName` option, then `unknown`, with every other pair of `OTEL_RESOURCE_ATTRIBUTES`. Before each push it asks the registered telemetry token source for its bearer; a static `bearer` option wins. A credential failure drops the batch, and the first credential failure and the first refusal each log one warning, as for metrics.

## Lifecycle

| Phase | Behavior |
|---|---|
| `warmup` | Initializes the collector, registers the middleware outermost on every composed HTTP server |
| `start` | Starts the periodic flush timer |
| `stop` | Drains all remaining buckets (including the current second), POSTs a final OTLP/JSON request, clears the timer |

The flush timer is `unref()`'d so it does not keep the process alive. Telemetry failures never affect application behavior.

## Best Practices

1. **Use dot-delimited metric names** — e.g. `app.module.metric` for consistent namespacing.
2. **Keep counter cardinality bounded** — Avoid dynamic segments in metric names. Use route patterns, not resolved paths.
3. **Use histograms for durations, in milliseconds** — They give you count, sum, min, max and percentiles in a single metric.
4. **Set the `app` option** — Makes it easy to filter metrics per service in the backend.
5. **Prefer a token source to a static bearer** — It renews expiring tokens; never hardcode tokens in source code.

## Troubleshooting

### Metrics not appearing in the backend

- Without an explicit plugin, check that `OTEL_EXPORTER_OTLP_ENDPOINT` is set and `OTEL_SDK_DISABLED` is not `true`.
- With an explicit plugin, check that `enabled` is `true` and `endpoint` is set. If the endpoint is missing, the logs show `telemetry is enabled but no endpoint is configured`.
- A `the telemetry collector refused a push` warning means the credential is wrong: on Putnami Cloud, check that `src/activate.ts` calls `register()` from `@putnami/cloud/runtime`.
- A `telemetry could not obtain a credential` warning means the token source failed.
- Check application logs for `telemetry flush every Ns` at startup — if missing, the plugin did not initialize.

### High memory usage

The collector only keeps the current second's bucket plus any unflushed completed buckets. If the flush fails repeatedly, completed buckets accumulate. Ensure the endpoint is reachable.

### Metric names contain resolved paths instead of patterns

The middleware uses `ctx.route` (the matched route pattern). If a request does not match any route, the raw path is used as a fallback. Register routes before the telemetry middleware to ensure patterns are available.
