# ADR 0007 — Telemetry is on by default when the environment names a collector

- **Status**: accepted
- **Scope**: `@putnami/application` (`telemetry/`, `application()`), with the
  telemetry token-source registry in `@putnami/runtime` and its Google Cloud
  source in `@putnami/cloud/runtime`

## Context

A TypeScript service exports metrics and traces only when its entry point adds
`telemetry({ enabled: true, endpoint })`, and logs only when it builds its own
logger around an `OtlpLogSink`. No deployed site does. The platform
already sets `OTEL_EXPORTER_OTLP_ENDPOINT` and `OTEL_RESOURCE_ATTRIBUTES` on
every workload, but the plugin reads neither.

Even a service that wires the plugin by hand cannot reach a hosted collector.
The collector admits a Google ID token whose audience is its own URL, and such
a token expires after an hour. The plugin only accepts a static `bearer`, read
once at startup, and it drops every refused push without a word.

The Go framework settled the same gap in
[Go telemetry ADR 0003](../../../../../go/framework/telemetry/doc/adr/0003-telemetry-is-on-by-default-from-the-environment.md).

## Decision

1. **The standard variables decide.** When the environment sets
   `OTEL_EXPORTER_OTLP_ENDPOINT`, `application()` adds a telemetry plugin
   pointed at it. `OTEL_SDK_DISABLED=true` turns it off. Without an endpoint
   the application gets no plugin and nothing leaves the process.
2. **An explicit plugin owns telemetry.** When the composition holds a
   `telemetry()` plugin, the environment adds nothing. That plugin's own
   configuration decides, so a request is counted once.
3. **The service identity comes from the environment.** `service.name` is
   `OTEL_SERVICE_NAME`, then the `service.name` pair of
   `OTEL_RESOURCE_ATTRIBUTES`, then the plugin's `app` option, then `unknown`.
   Every other pair of `OTEL_RESOURCE_ATTRIBUTES` reaches the resource of each
   request.
4. **The middleware goes outermost on every server.** The plugin puts its
   middleware in front of every other one on each `HttpPlugin` the
   application composes, in a sub-module too, so a request an inner
   middleware refuses is still counted and traced. A server another plugin
   creates during warmup counts too: `prepare()` instruments again once every
   plugin has warmed up, whatever the composition order. It creates no
   server: an application without one keeps its collector for custom
   metrics.
5. **A credential is asked for at each push.** `@putnami/runtime` holds one
   process-wide telemetry token source, a function of the collector endpoint.
   A runtime package registers it; the plugin calls it before each push, and
   the source caches. A static `bearer` wins over it. When the source fails,
   the batch is dropped: nothing is sent without a credential.
6. **A refusal is visible once.** The first `401` or `403` from the collector
   and the first credential failure each log one warning. Telemetry never
   fails the application.
7. **The Cloud runtime supplies the Google token.** `register()` from
   `@putnami/cloud/runtime`, which every Cloud workload already calls, also
   registers a source that returns a Google ID token for the collector URL on
   Google Cloud and nothing elsewhere.
8. **Logs follow the same environment.** When the environment names a
   collector, `application()` attaches an `OtlpLogSink` to the root logger
   when it starts, unless that logger already holds one the application
   configured. Applications started in one process share that sink; the last
   to stop, or to fail its start, detaches and closes it, which flushes its
   buffer, and a `run()` runner flushes it when it returns.
   Every `OtlpLogSink` takes the service identity of point 3 and asks the
   token source of point 5 before each push, with the warnings of point 6.
   An explicit `telemetry()` plugin exports no logs, so it does not stop this
   sink: `OTEL_SDK_DISABLED=true` turns logs off with the other signals. The
   root logger gains `addSink()` and a read-only `sinks` in
   `@putnami/runtime` for this.

## Rejected alternatives

- **A default-plugin registry, as in Go.** `@putnami/cloud` does not depend on
  `@putnami/application` and should not start to. Reading the standard
  variables in the framework needs no registry, and only the credential is
  platform-specific.
- **Have the platform inject a token.** Tokens expire, and the platform stamps
  only the endpoint on purpose.
- **Read the endpoint from the `telemetry` config section.** The platform
  writes the standard variable name, and a second name for the same value
  would drift.
- **Send without a credential when the source fails.** The collector refuses
  it anyway, and a credential failure would look like a collector failure.
- **Let the environment telemetry plugin own the log sink.** That plugin is
  absent whenever the composition holds `telemetry()`, which exports no logs,
  so an application with an explicit plugin would lose its logs.
- **Replace the root logger with one that adds the sink.** Its sinks are not
  readable, a logger derived before the swap would keep writing to the old
  sinks only, and stop would have to restore a logger someone else may have
  replaced meanwhile.

## Consequences

- Every TypeScript workload on Putnami Cloud exports request metrics, traces
  and logs once its framework version carries this change. It needs no code
  change.
- A workload outside Putnami Cloud that sets `OTEL_EXPORTER_OTLP_ENDPOINT`
  exports too, as any OpenTelemetry SDK would. `OTEL_SDK_DISABLED=true` stops
  it.
- A test that sets `OTEL_EXPORTER_OTLP_ENDPOINT` gets telemetry in every
  application it builds, and a process that runs two such applications gets
  the warning that the metrics helpers support one telemetry application per
  process.
