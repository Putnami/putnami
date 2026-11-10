# ADR 0003 — Telemetry is on by default when the environment names a collector

- **Status**: accepted
- **Scope**: `go.putnami.dev/telemetry` (`go/framework/telemetry`), with
  `app.RegisterDefaultPlugin` in `go.putnami.dev/app` and
  `ServerPlugin.UseOutermost` in `go.putnami.dev/http`

## Context

A service records request metrics and traces only when its main package
installs `NewPlugin` and adds `HTTPMiddleware` to each server. Most services
never do, so a platform that sets `OTEL_EXPORTER_OTLP_ENDPOINT` still receives
nothing from them. The fix must reach every service without editing each main
package, and must not break a service that already wires telemetry itself.

## Decision

`FromEnvironment` builds the telemetry a service gets without asking. A runtime
package registers it once with `app.RegisterDefaultPlugin`, and `app.New` adds
it before the workload's own plugins.

1. **The standard variables decide.** `OTEL_EXPORTER_OTLP_ENDPOINT` turns it on,
   `OTEL_SDK_DISABLED=true` turns it off, and the service identity comes from
   `OTEL_SERVICE_NAME` and `OTEL_RESOURCE_ATTRIBUTES`. Without an endpoint the
   factory returns nil and the application has no extra plugin.
2. **The middleware goes outermost.** The plugin adds `HTTPMiddleware` in front
   of every middleware already on each `http.ServerPlugin`, so a request that
   an inner middleware refuses is still counted and traced.
3. **A request counts once.** The middleware marks the request context, and a
   second `HTTPMiddleware` on the same request passes through. A service that
   already adds the middleware keeps working and records no duplicate.
4. **An explicit plugin owns the providers.** When the application installs
   `NewPlugin`, the default plugin builds no providers and only instruments the
   servers.
5. **The default plugin registers no DI tokens.** It cannot collide with the
   tokens of an explicit `NewPlugin`.
6. **Instruments are created on the first request.** The middleware binds to
   the global providers that exist when traffic starts, not to the ones that
   existed when the server was built.

## Rejected alternatives

- **Edit every main package.** Every new service would have to remember it,
  and the gap would come back.
- **Install the providers in a runtime package `init()`.** No lifecycle stops
  them, so the last batch is lost on shutdown, and the servers stay
  uninstrumented.
- **Add the middleware innermost, with `Use`.** A request refused by
  authentication or rate limiting would vanish from the metrics.
- **A framework-wide switch in `app` for telemetry.** `app` cannot import
  `telemetry`; a registry of default plugins keeps the dependency direction.

## Consequences

- A service that imports the runtime package exports request metrics and
  traces as soon as its environment names a collector.
- A test that sets `OTEL_EXPORTER_OTLP_ENDPOINT` and builds an application with
  that runtime package imported gets the telemetry plugin too.
- Default plugins are process-wide. A package that registers one affects every
  application the process builds.
