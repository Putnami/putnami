# Telemetry

The `TelemetryPlugin` collects application metrics — HTTP request counts, response durations, error rates — and pushes them to an OTLP-style telemetry backend of your choice. You can also record custom counters, gauges, and histograms.

> Telemetry is opt-in. It is disabled by default and has no default endpoint, so no operational data leaves the process unless you explicitly set `enabled: true` and configure an `endpoint`.

## Getting started

Register the `telemetry()` plugin alongside `http()`:

```ts
import { application, http, telemetry } from '@putnami/application';

const app = application()
  .use(
    telemetry({
      enabled: true,
      endpoint: process.env.TELEMETRY_ENDPOINT,
      bearer: process.env.TELEMETRY_TOKEN,
    }),
  )
  .use(http({ port: 3000 }));

await app.start();
```

Once enabled with an endpoint, HTTP metrics are collected automatically. No further setup is needed.

## What is collected automatically

The plugin registers a middleware that instruments every HTTP request:

| Metric | Type | Name pattern | Example |
|---|---|---|---|
| Request count | Counter | `http.{METHOD}.{route}.{status}` | `http.GET./api/users.200` |
| Response time | Histogram | `http.{METHOD}.{route}.duration` | `http.GET./api/users.duration` |
| Client errors | Counter | `http.error.4xx` | — |
| Server errors | Counter | `http.error.5xx` | — |

Routes use the pattern (`/users/[id]`), not the resolved path (`/users/123`), to keep metric cardinality bounded.

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

// Histogram — distribution (count, sum, min, max per second)
const start = Date.now();
await processOrder(order);
observeHistogram('app.order.processing.duration', Date.now() - start);
```

## Configuration

```ts
telemetry({
  enabled: true,
  endpoint: 'https://otel.example.com/v1/runtime',
  bearer: 'my-token',
  flushIntervalS: 30,
  app: 'my-service',
});
```

| Option | Type | Default | Description |
|---|---|---|---|
| `enabled` | `boolean` | `false` | Enable telemetry collection (explicit opt-in) |
| `endpoint` | `string` | — | Telemetry ingest URL; required when enabled |
| `bearer` | `string` | — | Bearer token for authentication |
| `flushIntervalS` | `number` | `30` | Seconds between flushes |
| `app` | `string` | — | Application identifier |

If `enabled` is `true` but `endpoint` is missing, the plugin logs a warning and stays inert. It never falls back to a hardcoded destination.

Or via YAML:

```yaml
telemetry:
  enabled: true
  endpoint: "https://otel.example.com/v1/runtime"
  bearer: "..."
  flushIntervalS: 30
  app: "my-service"
```

## How aggregation works

Metrics are bucketed by Unix second in memory. A periodic timer drains completed buckets, builds a JSON payload, and POSTs it to the endpoint. On shutdown, all remaining buckets (including the current second) are flushed immediately.

The telemetry backend can adjust the flush interval dynamically by returning `{ "flushIntervalS": N }` in the response. The plugin applies it without restart.

All telemetry failures are silently dropped — they never affect application behavior.

## Related guides

- [Persistence — SQL observability](/docs/frameworks/typescript/persistence#observability)
- [Health checks](/docs/frameworks/typescript/health-checks)
- [Plugins & lifecycle](/docs/frameworks/typescript/plugins-and-lifecycle)
- [HTTP & middleware](/docs/frameworks/typescript/http-and-middleware)
- [Add observability](/docs/how-to/add-observability)
