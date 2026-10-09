# Platform Endpoints

`go.putnami.dev/platform` mounts the operational HTTP surface every Putnami
workload needs — liveness, health, readiness, build metadata, and optional
profiling — and discovers per-plugin probes from the application's module tree.

## Overview

The module provides three things:

1. **A fixed endpoint set** so one orchestrator configuration works for every Go
   service.
2. **Probe aggregation** driven by the `app.HealthChecker` and
   `app.ReadinessChecker` interfaces that plugins already implement.
3. **Build metadata** served from configured values, falling back to the
   information the Go toolchain embedded in the binary.

## Quick Start

```go
package main

import (
    "go.putnami.dev/app"
    "go.putnami.dev/http"
    "go.putnami.dev/platform"
)

func main() {
    server := http.NewServerPlugin(http.ServerConfig{Port: 8080})

    ops := platform.NewPlugin(platform.Config{
        Version: platform.VersionInfo{Name: "my-service", Version: "1.0.0"},
    })

    a := app.New("my-service")
    a.Use(server)
    a.Use(ops)
    if err := a.ListenAndServe(); err != nil {
        panic(err)
    }
}
```

`Use(ops)` does two jobs:

- It mounts the routes. When the application configures, the plugin registers
  its endpoints on the application's single `http.ServerPlugin`, in any plugin
  order.
- It wires the lifecycle. `/healthz` answers `unavailable` until `Start` sets
  the running flag. `/readyz` also waits until the application completed
  startup: every plugin `Start` and every module `OnStart` hook returned. A
  failed startup never makes it ready.

An application that holds no server, or several, fails configure with an error
that names `RegisterOn`. Call `ops.RegisterOn(server)` before the application
configures to choose the server; the plugin then registers nothing more.

Then:

```bash
curl -s localhost:8080/livez     # {"status":"ok"}
curl -s localhost:8080/healthz   # {"status":"ok"} or a degraded envelope
curl -s localhost:8080/readyz    # readiness aggregate
curl -s localhost:8080/version   # build metadata
```

## Choosing the right endpoint

| Probe | Endpoint | Why |
|-------|----------|-----|
| Kubernetes `livenessProbe` | `/livez` | Runs nothing. A slow dependency must not restart a healthy process. |
| Kubernetes `readinessProbe` | `/readyz` | Aggregates readiness probes, so traffic drains while a dependency is down. |
| Dashboards and on-call checks | `/healthz` | Aggregates liveness probes with per-probe detail. |

## Namespacing the paths

Set `Prefix` when the workload's public API also lives at root:

```go
platform.NewPlugin(platform.Config{Prefix: "/_"})
// mounts /_/livez, /_/healthz, /_/readyz, /_/version
```

Empty, `"/"`, and `"/_/"` normalise to `""`, `""`, and `"/_"` respectively.

## Contributing a probe

Most probes come for free. Any plugin implementing `app.HealthChecker` or
`app.ReadinessChecker` is discovered automatically, using the plugin's `Name()`:

```go
func (p *CachePlugin) Name() string { return "cache" }

func (p *CachePlugin) CheckHealth(ctx context.Context) error {
    return p.client.Ping(ctx)
}
```

Discovery runs on the first aggregate request, not during configure, so it does
not matter whether the plugin was registered before or after the platform plugin.

For a probe no plugin owns — an external URL, an ad-hoc check — register it
explicitly:

```go
if err := ops.AddReadinessChecker("billing-api", func(ctx context.Context) error {
    return pingBilling(ctx)
}); err != nil {
    return err
}
```

An explicit registration always wins over a discovered probe of the same name, so
a workload can override a plugin-provided probe.

Probe names must match `^[a-z0-9][a-z0-9_./-]{0,63}$`. Explicit registration
rejects a bad name immediately; a discovered or required bad name fails
application start, because the name is a key in the response envelope the
platform protocol validates.

## Failing closed on a missing dependency

`Config.Required` lists readiness probes the workload treats as mandatory:

```go
platform.NewPlugin(platform.Config{
    Required: []string{"database", "billing-api"},
})
```

If a required name is neither registered nor discovered by the time `/readyz`
runs, readiness reports `degraded` with a synthesized failing entry for that
name, so an incompletely wired workload drains instead of taking traffic.
`/healthz` deliberately ignores the list: a missing dependency is a readiness
question, not a reason to restart the process.

## Timeouts

Each probe runs in parallel under its own `ProbeTimeout` (5s by default). The
aggregate answers after a bounded grace period beyond that, so a probe that
ignores cancellation delays the response by a known amount rather than holding
the endpoint open.

## Probe metrics

The module takes no OpenTelemetry dependency, so it exposes a callback instead:

```go
type meterBridge struct{ /* … */ }

platform.NewPlugin(platform.Config{ProbeMetrics: &meterBridge{}})
```

Every probe outcome and latency, plus each aggregate result, flows through it.
Left nil, the callbacks are skipped entirely.

## Redacting probe errors

By default a failing probe's error text is returned verbatim, which is what the
platform protocol specifies. Because the endpoints are unauthenticated and mount
at root, enable redaction when they are reachable from an untrusted network:

```go
platform.NewPlugin(platform.Config{RedactProbeErrors: true})
```

The caller then sees a generic message and the real cause is logged server-side
with the request's trace context.

## Profiling

```go
platform.NewPlugin(platform.Config{EnablePprof: true})
```

Off by default: pprof exposes heap and goroutine internals and is not safe on a
public port.

## Version metadata

`/version` returns the configured `VersionInfo`. Empty fields fall back to
`runtime/debug.ReadBuildInfo`, which supplies the module path plus the VCS
revision and time when the binary was built with `-buildvcs`. A generated
`version.json` can be parsed with `platform.VersionFromGenerated`.

## Best practices

- **Point liveness at `/livez` and readiness at `/readyz`.** Aggregating probes
  on the liveness path turns a slow dependency into a restart loop.
- **Let plugins own their probes.** Auto-discovery keeps the wiring in the plugin
  that knows what healthy means.
- **List genuinely mandatory dependencies in `Required`.** It converts a silent
  wiring mistake into a workload that never takes traffic.
- **Namespace with `Prefix` when the API lives at root**, so an operational path
  can never shadow a product route.
- **Enable `RedactProbeErrors` on internet-reachable deployments**, and keep the
  verbatim default behind a trusted boundary.

## Contract and compatibility

See the [platform endpoints specification](../specs/platform-endpoints.json), the
[probe-discovery ADR](adr/0001-probe-discovery-happens-on-the-request-path.md),
and [support evidence](../README.md#support-and-contract). The package is stable
and maintained, with the workspace's [pre-1.0 migration
policy](../../../../RELEASE.md), not strict compatibility between every `0.x`
minor.
