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
| `OTEL_METRIC_EXPORT_INTERVAL` | Milliseconds between two metric pushes when the plugin sets no interval. Default 60000. |
| `OTEL_BSP_SCHEDULE_DELAY` | Longest wait, in milliseconds, of a completed span before its batch is pushed, when the plugin sets no delay. Default 30000. |
| `OTEL_BSP_MAX_EXPORT_BATCH_SIZE` | Spans that make a full batch, pushed at once. Default 512. |

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
  metricIntervalS: 60,
  spanBatchDelayS: 30,
  app: 'my-service',
});
```

| Option | Type | Default | Description |
|---|---|---|---|
| `enabled` | `boolean` | `false` | Enable the explicit plugin (opt-in) |
| `endpoint` | `string` | — | OTLP/HTTP collector base URL; metrics go to `<endpoint>/v1/metrics`, spans to `<endpoint>/v1/traces`. Required when enabled |
| `bearer` | `string` | — | Static bearer token; wins over the registered token source |
| `metricIntervalS` | `number` | `60` | Seconds between two metric pushes; `OTEL_METRIC_EXPORT_INTERVAL` (milliseconds) sets it when unset |
| `spanBatchDelayS` | `number` | `30` | Longest wait, in seconds, of a span before its batch is pushed; 512 spans push at once. `OTEL_BSP_SCHEDULE_DELAY` (milliseconds) sets it when unset |
| `app` | `string` | — | `service.name` when neither `OTEL_SERVICE_NAME` nor a `service.name` resource attribute is set; `unknown` without all three |
| `traces` | `Partial<TraceSampling>` | budget `100`, ratio `0.01`, floor `1` per minute | Trace budget; see [Trace budget](#trace-budget) |

`metricIntervalS` and `spanBatchDelayS` replace `flushIntervalS`, which pushed metrics and spans together every 30 seconds. The configuration ignores a `flushIntervalS` key: rename it to `metricIntervalS`.

If `enabled` is `true` but `endpoint` is missing, the plugin logs a warning and stays inert. It never falls back to a hardcoded destination.

Or via YAML:

```yaml
telemetry:
  enabled: true
  endpoint: "https://collector.example:4318"
  metricIntervalS: 60
  app: "my-service"
```

## Trace budget

Each instance keeps its first `budget` root traces, then keeps a root when
`ratio` picks its trace ID, and keeps at least `floorPerMinute` roots each UTC
minute, so a rarely called service still exports traces. A span with a parent,
remote or ambient, takes its parent's sampled flag: a service follows its
caller's decision. The Go runtime applies the same rule to the same keys.

| Key in `telemetry.traces` | Default | Meaning |
|---|---|---|
| `budget` | `100` | Root traces the instance keeps first |
| `ratio` | `0.01` | Share of root traces kept once the budget is spent, from 0 to 1 |
| `floorPerMinute` | `1` | Root traces kept each minute whatever the ratio picks |

```yaml
# conf/.env.yaml, or CONFIG_DATA
telemetry:
  traces:
    budget: 100
    ratio: 0.01
    floorPerMinute: 1
```

The `traces` option of `telemetry()` sets the same three values in code; each
key of the config section overrides its field. While it runs, the plugin
reloads the config sources every 30 seconds with `readConfig()` and applies a
changed value to the next root decision, without a restart. A change to a file
under `conf/` applies within 30 seconds. `CONFIG_DATA` and the Putnami Cloud
remote sources keep their first load for the life of the process: to change a
running process there, call `sampler.update()`. A value out of range, or a
section that fails to load, keeps the settings in force and logs a warning.

A dropped span records nothing, and still injects its `traceparent` with the
flag `00`, so the services it calls drop the trace too. Each root decision
counts in the counter `trace.decisions`, labeled `outcome` `kept` or
`dropped`. Without a telemetry plugin, a root span is sampled.

A proxy between two services may insert a span it never exports and rewrite
the parent ID of `traceparent`. A client span therefore injects its
`traceparent` value twice, the second time in `x-putnami-traceparent`, which
proxies leave alone. When both headers carry the same trace ID and different
parent IDs, the server span parents to the one in `x-putnami-traceparent`, so
the trace keeps the call as one edge. When the second header is absent,
malformed or on another trace, the server uses `traceparent`. The Go runtime
applies the same rule. A WebSocket stream carries its context in the init
frame, which proxies do not rewrite, so it needs no second header.

An event carries the trace too: a publish inside a span writes the span's
`traceparent` and `tracestate` into the event attributes, and `@putnami/events`
runs each handler in a consumer span, `process <topic>`, that continues that
trace, or starts a new root for an event without one. A TypeScript publisher
and a Go consumer share the trace.

`TelemetryPlugin.sampler` (or `getTraceSampler()`) returns the installed
`TraceSampler`. Its `settings` are its own, without an export directive's
limits, which `inForce()` adds. Its `update()` tightens the settings at
runtime, and they hold until the `telemetry.traces` section changes.

## Export directives

The OTLP endpoint can tighten the limits of every service that pushes to it,
without a redeploy. Any push response, from the metrics, traces or logs path
and whatever its status, may carry a `Putnami-Export-Directive` header:

```
Putnami-Export-Directive: trace-ratio=0.001, trace-floor=1, log-level=warn, ttl=300
```

| Key | Value | Effect while the directive holds |
|---|---|---|
| `trace-ratio` | a number from 0 to 1 | the lower of it and `telemetry.traces.ratio` |
| `trace-floor` | a whole number of 0 or more | the lower of it and `telemetry.traces.floorPerMinute` |
| `log-level` | `debug`, `info`, `warn` or `error` | the higher of it and the logger's own level |
| `ttl` | whole seconds, required, capped at 3600 | how long the directive holds |

A directive only tightens: the service keeps its own `budget`, a looser value
changes nothing, and an `error` record is always emitted. A valid directive
replaces the one in force; when its `ttl` ends, the service returns to its own
settings. A member without `=`, a malformed known value, a repeated key or a
missing `ttl` voids the header, and an unknown key is ignored. The directive limits the installed
`TraceSampler`, whose `settings` keep their own values, and every logger that
checks the level through `setLogLevelLimit()`, without changing the live
level. The roots and records it drops still count in `trace.decisions` and
`log.records`.

A `429 Too Many Requests` with a `Retry-After`, in seconds or as an HTTP date,
delays the next push of that signal to that collector, from every exporter
of the process, by at most an hour. A metrics or span
batch due during the delay is dropped; log records wait in the `OtlpLogSink`
queue, and a shutdown during the delay drops them with a warning. The Go
runtime reads the same header by the same rule.

## How aggregation works

Metrics are bucketed by Unix second in memory. Every 60 seconds a timer drains completed buckets, renders them as an OTLP/JSON request, and POSTs it to `<endpoint>/v1/metrics`; completed spans go to `<endpoint>/v1/traces` in batches at most 30 seconds apart, or at once when 512 wait. On shutdown, all remaining buckets (including the current second) and spans are flushed immediately.

Telemetry failures never affect application behavior.

## Related guides

- [Persistence — SQL observability](/docs/frameworks/typescript/persistence#observability)
- [Health checks](/docs/frameworks/typescript/health-checks)
- [Plugins & lifecycle](/docs/frameworks/typescript/plugins-and-lifecycle)
- [HTTP & middleware](/docs/frameworks/typescript/http-and-middleware)
- [Add observability](/docs/how-to/add-observability)
