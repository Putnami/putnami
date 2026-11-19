# Telemetry

The `TelemetryPlugin` collects application metrics and spans — HTTP request counts, response durations, error rates — and pushes them as OTLP/JSON over HTTP to an OpenTelemetry collector. You can also record custom counters, gauges, and histograms.

## Getting started

### From the environment

When the environment sets `OTEL_EXPORTER_OTLP_ENDPOINT`, every `application()` adds the telemetry plugin by itself and pushes to that collector. No code is needed: Putnami Cloud sets this variable on every workload, with the URL of its workspace collector.

| Variable | Effect |
|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | Collector base URL. Without it, the application gets no telemetry and nothing leaves the process. |
| `OTEL_SDK_DISABLED=true` | Turns this default telemetry off, whatever the endpoint says. |
| `OTEL_SERVICE_NAME` | The `service.name` of every pushed resource; without it, the `service.name` pair of `OTEL_RESOURCE_ATTRIBUTES`, then the plugin's `app` option or the log sink's `serviceName` option. |
| `OTEL_RESOURCE_ATTRIBUTES` | Comma-separated `key=value` pairs added to every pushed resource. |

The same variables export logs. `application()` attaches an `OtlpLogSink` to the root logger when it starts, so every record reaches `<endpoint>/v1/logs` with the resource and the credentials of metrics and traces. Applications started in one process share that sink, a `run()` runner flushes it when it returns, and the last application to stop closes it. A root logger that already holds an `OtlpLogSink` the application configured keeps it alone.

### Explicitly

Register the `telemetry()` plugin to configure telemetry yourself. An explicit plugin owns metrics and traces: the environment then adds no second plugin, and the plugin's own configuration decides. It exports no logs, so the log sink the environment attaches stays; `OTEL_SDK_DISABLED=true` turns it off.

```ts
import { application, http, telemetry } from '@putnami/application';

const app = application()
  .use(
    telemetry({
      enabled: true,
      endpoint: process.env.TELEMETRY_ENDPOINT,
    }),
  )
  .use(http({ port: 3000 }));

await app.start();
```

> An explicit plugin is opt-in. It is disabled by default and has no default endpoint, so it sends nothing unless you set `enabled: true` and an `endpoint`.

Once telemetry runs, HTTP metrics are collected automatically. No further setup is needed.

## Credentials

Before each push, the plugin and the log sink ask the telemetry token source registered with `@putnami/runtime` for a bearer token, so a token that expires is renewed without a restart. On Putnami Cloud, `register()` from `@putnami/cloud/runtime`, which a Cloud workload already calls in its `activate.ts`, registers a source that returns a Google ID token for the collector.

```ts
import { registerTelemetryTokenSource } from '@putnami/runtime';

registerTelemetryTokenSource(async (endpoint) => fetchTokenFor(endpoint));
```

A static `bearer` option wins over the source. When the source fails, the batch is dropped rather than sent without a credential. The first collector refusal (`401` or `403`) and the first credential failure each log one warning.

## What is collected automatically

The plugin registers a middleware in front of every other one on each HTTP server the application composes, so a request that an inner middleware refuses (for example the origin guard) is still counted and traced:

`http.server.request_count` and `http.server.duration`, labeled by method, route pattern and status code, the same as the Go runtime. [Add observability](/docs/how-to/add-observability#http-server-metrics) lists them, with their unit and histogram buckets.

### SQL metrics (via `@putnami/database`)

When the `sql()` plugin is used alongside `telemetry()`, SQL metrics are collected automatically:

| Metric | Type | Name pattern | Example |
|---|---|---|---|
| Query count | Counter | `sql.{operation}.{table}` | `sql.find.users` |
| Query duration | Histogram | `sql.{operation}.{table}.duration` | `sql.save.orders.duration` |
| Aggregate duration | Histogram | `sql.query.duration` | — |
| Query errors | Counter | `sql.{operation}.{table}.error` | `sql.save.users.error` |
| Aggregate errors | Counter | `sql.query.error` | — |
| Slow queries | Counter | `sql.query.slow` | — |
| Pool created | Counter | `sql.pool.created` | — |
| Pool closed | Counter | `sql.pool.closed` | — |
| Pool count | Gauge | `sql.pool.count` | — |

Operations: `find`, `save`, `saveMany`, `delete`, `deleteMany`, `exists`.

## Custom metrics

Three metric types are available. They are no-ops when telemetry is disabled, so you can call them unconditionally.

```ts
import { incCounter, setGauge, observeHistogram } from '@putnami/application';

// Counter — monotonically increasing
incCounter('app.orders.created');
incCounter('app.emails.sent', 3);

// Gauge — point-in-time value
setGauge('app.queue.size', pendingJobs.length);

// Histogram — distribution (count, sum, min, max and bucket counts per second)
const start = Date.now();
await processOrder(order);
observeHistogram('app.order.processing.duration', Date.now() - start);
```

## Configuration

```ts
telemetry({
  enabled: true,
  endpoint: 'https://collector.example:4318',
  bearer: 'my-token',
  flushIntervalS: 30,
  app: 'my-service',
});
```

| Option | Type | Default | Description |
|---|---|---|---|
| `enabled` | `boolean` | `false` | Enable the explicit plugin (opt-in) |
| `endpoint` | `string` | — | OTLP/HTTP collector base URL; metrics go to `<endpoint>/v1/metrics`, spans to `<endpoint>/v1/traces`. Required when enabled |
| `bearer` | `string` | — | Static bearer token; wins over the registered token source |
| `flushIntervalS` | `number` | `30` | Seconds between flushes |
| `app` | `string` | — | `service.name` when neither `OTEL_SERVICE_NAME` nor a `service.name` resource attribute is set; `unknown` without all three |

If `enabled` is `true` but `endpoint` is missing, the plugin logs a warning and stays inert. It never falls back to a hardcoded destination.

Or via YAML:

```yaml
telemetry:
  enabled: true
  endpoint: "https://collector.example:4318"
  flushIntervalS: 30
  app: "my-service"
```

## How aggregation works

Metrics are bucketed by Unix second in memory. A periodic timer drains completed buckets, renders them as an OTLP/JSON request, and POSTs it to `<endpoint>/v1/metrics`; completed spans go to `<endpoint>/v1/traces`. On shutdown, all remaining buckets (including the current second) are flushed immediately.

Telemetry failures never affect application behavior.

## Related guides

- [Persistence — SQL observability](/docs/frameworks/typescript/persistence#observability)
- [Health checks](/docs/frameworks/typescript/health-checks)
- [Plugins & lifecycle](/docs/frameworks/typescript/plugins-and-lifecycle)
- [HTTP & middleware](/docs/frameworks/typescript/http-and-middleware)
- [Add observability](/docs/how-to/add-observability)
