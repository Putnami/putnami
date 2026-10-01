# go.putnami.dev/platform

Standard operational HTTP endpoints — liveness, health, readiness, version, and
optional pprof — with probe auto-discovery from the application's module tree.

## Quick Start

```go
import (
    "go.putnami.dev/app"
    "go.putnami.dev/http"
    "go.putnami.dev/platform"
)

server := http.NewServerPlugin(http.ServerConfig{Port: 8080})
ops := platform.NewPlugin(platform.Config{
    Version: platform.VersionInfo{Name: "my-service", Version: "1.0.0"},
})

a := app.New("my-service")
a.Use(server)
a.Use(ops) // mounts the routes on the server and wires the lifecycle
a.ListenAndServe()
```

`Use` is the only wiring step. At configure, the plugin mounts its routes on the
application's single `http.ServerPlugin`, in any plugin order. An application
with no server, or with several, fails configure with an error that names
`RegisterOn`. Call `ops.RegisterOn(server)` before the application configures to
choose the server; the plugin then registers nothing more.

## Endpoints

| Path | Behavior |
|------|----------|
| `GET /livez` | `200 {"status":"ok"}` whenever the handler can run. No probes, no flags. Use it as the Kubernetes liveness probe. |
| `GET /healthz` | `503 {"status":"unavailable"}` before `Start` and after `Stop`; `503 {"status":"degraded","checks":{…}}` when an `app.HealthChecker` probe fails; otherwise `200`. |
| `GET /readyz` | Same shape, driven by `app.ReadinessChecker` probes plus `Config.Required`. Use it as the Kubernetes readiness probe. |
| `GET /version` | `VersionInfo` as JSON; empty fields fall back to `runtime/debug.ReadBuildInfo`. |
| `GET /debug/pprof/*` | `net/http/pprof` index and profiles. Off unless `Config.EnablePprof` is true. |

`Config.Prefix` namespaces every path (`Prefix: "/_"` gives `/_/healthz`, …).

## Surface

- `platform.NewPlugin(platform.Config{…}) *Plugin` — constructor
- `platform.Plugin.RegisterOn(*http.ServerPlugin)` — mounts the routes on a server you choose; optional when the application holds exactly one server
- `platform.Plugin.AddHealthChecker(name string, fn HealthCheckFunc) error` — explicit liveness probe; rejects a name outside `^[a-z0-9][a-z0-9_./-]{0,63}$`
- `platform.Plugin.AddReadinessChecker(name string, fn HealthCheckFunc) error` — explicit readiness probe
- `platform.VersionInfo` — alias of the platform protocol's version payload
- `platform.VersionFromGenerated([]byte) (VersionInfo, error)` — parses a generated `version.json`
- `platform.ProbeMetrics` — callback seam for per-probe outcome and latency
- `platform.CodeInvalidProbeName` — error code for a non-conforming probe name

## Config

| Field | Default | Purpose |
|-------|---------|---------|
| `Prefix` | `""` (root) | Namespace for every mounted path |
| `EnablePprof` | `false` | Expose `/debug/pprof/*` |
| `Version` | zero | Build metadata for `/version` |
| `ProbeTimeout` | protocol default (5s) | Per-probe timeout |
| `Required` | none | Readiness probe names that must exist; a missing one degrades `/readyz` |
| `ProbeMetrics` | `nil` | Receives per-probe and aggregate outcomes |
| `RedactProbeErrors` | `false` | Replace a failing probe's error with a generic message in the response and log the real cause |

## Probe discovery

Any plugin in the module tree implementing `app.HealthChecker` or
`app.ReadinessChecker` is discovered automatically, on the first aggregate
request rather than during configure — so registration order does not matter.
An explicitly registered probe always wins over a discovered one of the same
name.

```go
func (p *MyPlugin) Name() string { return "upstream" }
func (p *MyPlugin) CheckHealth(ctx context.Context) error { /* … */ }
```

Probes run in parallel, each under its own timeout, and the aggregate answers
after a bounded grace period even when a probe ignores cancellation.

## Relationship to `http.HealthPlugin`

`http.NewHealthPlugin()` mounts a single `GET /_/health`. The platform plugin is
the richer alternative, not a superset wrapper. Both can share one server: each
discovers the same `app.HealthChecker` probes and reports them on its own route.
The `go-server` starter composes both: `putnami qualify` and deployment probes
wait on `/readyz`, and `/_/health` serves clients that probe it.

## Maintained contract

This public package is `stable` in the workspace [support
catalog](../../../putnami.support.json). Its behavior is defined by the [platform
endpoints specification](specs/platform-endpoints.json) and the
[probe-discovery ADR](doc/adr/0001-probe-discovery-happens-on-the-request-path.md).
Before v1.0, follow the workspace [migration-based compatibility
policy](../../../RELEASE.md); do not infer strict compatibility between every
`0.x` minor.

See `doc/getting-started.md` for full reference.
