# Platform Endpoints

`go.putnami.dev/platform` mounts the standard operational HTTP surface every Putnami workload needs — liveness, readiness, version metadata, and optional pprof — and discovers per-plugin probes from the application's module tree.

It does not replace `go.putnami.dev/http`'s `HealthPlugin`; it is a separate plugin you opt in to instead, mounting a richer endpoint set under a configurable prefix.

## Endpoints

| Path        | Purpose                                                                                                                  |
| ----------- | ------------------------------------------------------------------------------------------------------------------------ |
| `/livez`    | Lightweight liveness — returns `200 {"status":"ok"}` whenever the handler can run. No probes, no flags. Safe to use as a Kubernetes liveness probe. |
| `/healthz`  | Liveness aggregate — `200` when running and every `app.HealthChecker` probe passes; `503 {"status":"unavailable"}` before `Start` and after `Stop`; `503 {"status":"degraded","checks":{…}}` when any probe fails. |
| `/readyz`   | Readiness aggregate — same shape as `/healthz` but driven by `app.ReadinessChecker` probes. The right endpoint for a Kubernetes readiness probe. |
| `/version`  | Build metadata as JSON. Returns the `VersionInfo` you configure; empty fields fall back to `runtime/debug.ReadBuildInfo` (module path, VCS revision and time when built with `-buildvcs`). |
| `/debug/pprof/*` | `net/http/pprof` index and profile handlers. Disabled by default — set `Config.EnablePprof = true` to expose them. |

All paths are root by default. Set `Config.Prefix` to namespace them (e.g. `Prefix: "/_"` mounts `/_/healthz`, `/_/livez`, …).

## Quick Start

```go
import (
    "go.putnami.dev/app"
    "go.putnami.dev/http"
    "go.putnami.dev/platform"
)

server := http.NewServerPlugin(http.ServerConfig{Port: 8080})
platformPlugin := platform.NewPlugin(platform.Config{
    Version: platform.VersionInfo{Name: "my-service", Version: "1.0.0"},
})
platformPlugin.RegisterOn(server)

a := app.New("my-service").
    Use(server).
    Use(platformPlugin).
    Use(database.NewPlugin(database.PluginConfig{ /* … */ }))
a.ListenAndServe()
```

Hitting `GET /healthz` now returns:

```json
{
  "status": "ok",
  "checks": {"database": "ok"}
}
```

— the `database` probe shows up automatically because `database.Plugin` implements `app.HealthChecker`.

## Capability Interfaces

The platform plugin discovers two interfaces by walking the module tree from the root:

```go
// In go.putnami.dev/app
type HealthChecker interface {
    Plugin
    CheckHealth(ctx context.Context) error
}

type ReadinessChecker interface {
    Plugin
    CheckReadiness(ctx context.Context) error
}
```

Use `HealthChecker` for "dependency is broken in a way only a restart fixes" — Kubernetes liveness will restart the pod when it fails. Use `ReadinessChecker` for "dependency is temporarily down, drain traffic" — Kubernetes readiness will stop sending traffic without restarting.

A single plugin can implement both: `app.HealthChecker` contributes to `/healthz`, `app.ReadinessChecker` contributes to `/readyz`.

Implementations must be safe to call concurrently and respect context cancellation. The platform plugin runs every probe in parallel under a per-probe timeout (`Config.ProbeTimeout`, default `5s`) and bounds the whole response on that timeout plus a small grace: a probe that honours `ctx` returns its real error, and a probe that ignores cancellation is abandoned and reported as timed out rather than stalling the response. An abandoned probe's goroutine still runs to completion in the background (Go cannot preempt it) and a warning is logged, so a probe that never returns is a leak you should fix — but it can no longer hang `/healthz` or `/readyz`.

## Explicit Probes

For probes not owned by a plugin (an external URL, an ad-hoc check), register them directly. The name must match the canonical probe-name pattern `^[a-z0-9][a-z0-9_./-]{0,63}$`; `Add{Health,Readiness}Checker` returns a `CodeInvalidProbeName` error for a non-conforming name (a bad name would otherwise make `/healthz` emit an envelope that fails the protocol's own validator):

```go
if err := platformPlugin.AddHealthChecker("upstream", func(ctx context.Context) error {
    return checkUpstreamURL(ctx, "https://example.com/health")
}); err != nil {
    return err
}

if err := platformPlugin.AddReadinessChecker("warm", func(ctx context.Context) error {
    if !cacheWarmed.Load() {
        return errors.New("cache warming")
    }
    return nil
}); err != nil {
    return err
}
```

Explicit registrations win over auto-discovered probes of the same name — useful when you need to override a plugin-provided probe for a specific environment.

## Configuration

```go
type Config struct {
    Prefix       string        // path prefix (default: "" → root)
    EnablePprof  bool          // expose /debug/pprof/* (default: false)
    Version      VersionInfo   // /version payload
    ProbeTimeout time.Duration // per-probe timeout (default: 5s)
}

type VersionInfo struct {
    Name      string
    Version   string
    SHA       string
    Branch    string
    BuildTime string
}
```

### Build metadata

For production builds you typically embed version info at link time:

```go
var version = "dev"
var commit = ""

platform.NewPlugin(platform.Config{
    Version: platform.VersionInfo{
        Name:    "my-service",
        Version: version,
        SHA:     commit,
    },
})
```

Build with `go build -ldflags "-X main.version=1.2.3 -X main.commit=$(git rev-parse HEAD)"`.

If you leave `Version` empty, the plugin fills in what `runtime/debug.ReadBuildInfo` exposes (module path, plus `vcs.revision` / `vcs.time` when the binary was built with `-buildvcs=true` inside a clean repo).

#### From the generated `.gen/version.json`

The Putnami build pipeline writes `<project>/.gen/version.json` with the full
build metadata — `name`, `version`, `sha`, `branch`, `buildTime`, and `isDirty`.
`runtime/debug.ReadBuildInfo` structurally cannot report `branch` or `buildTime`,
so embed the generated file and hand it to `VersionFromGenerated` to surface
everything Putnami already records:

```go
import _ "embed"

//go:embed .gen/version.json
var versionJSON []byte

info, err := platform.VersionFromGenerated(versionJSON)
if err != nil {
    info = platform.VersionInfo{Name: "my-service"} // degrade gracefully
}
platform.NewPlugin(platform.Config{Version: info})
```

A dirty working tree is surfaced by appending `+dirty` to the reported `sha`
(the wire shape has no boolean dirty field). Any field the file leaves empty
still falls back to `runtime/debug.ReadBuildInfo` at serve time, so a partial
file degrades cleanly. The `.gen/version.json` is produced by `putnami build`
(see the version-file generation step) — it must exist at compile time for the
`go:embed` to succeed.

### Probe metrics

The aggregate `/healthz` and `/readyz` handlers can report per-probe outcome
and latency through an optional `Config.ProbeMetrics` sink. The platform module
takes no OpenTelemetry dependency itself; bridge the `ProbeMetrics` interface to
`telemetry.Meter` in your workload (counters/histograms/gauges on the global
meter provider stay a no-op until an exporter is installed). Left unset, probe
execution is unchanged. See the `ProbeMetrics` doc comment for a copy-paste
otel bridge.

### pprof

`/debug/pprof/*` exposes the runtime profiler's index, the named profiles (`heap`, `goroutine`, `allocs`, `block`, `mutex`, `threadcreate`), and the on-demand collectors (`cmdline`, `profile`, `symbol`, `trace`). It is off by default because profiles reveal heap layout and goroutine details, and the CPU/trace collectors are expensive.

Enable explicitly:

```go
platform.NewPlugin(platform.Config{EnablePprof: true})
```

Do not expose pprof on a public port. Mount the platform plugin on a separate admin server, or restrict access via a sidecar / network policy.

### Health endpoint error disclosure

`/healthz` and `/readyz` are unauthenticated and mounted at root by
default, and on a failing probe they return the probe's error **verbatim**
in the response body — this is an intentional cross-language protocol
contract (operators need the originating cause). Probe errors routinely
carry internal detail (`dial tcp 10.0.2.5:5432: connect: connection
refused`, DB driver text, internal hostnames), so an unauthenticated
caller can read internal topology whenever a dependency is unhealthy.

Apply the same mitigation as pprof: mount these endpoints behind an admin
prefix (`Config.Prefix`), on a separate admin server, or behind a
sidecar / network policy. If they must be reachable from an untrusted
network, set `Config.RedactProbeErrors: true` — failing probes then report
a generic `"unhealthy"` in the body while the verbatim cause is logged
server-side:

```go
platform.NewPlugin(platform.Config{RedactProbeErrors: true})
```

## Migrating from `http.HealthPlugin`

Workloads currently using `http.NewHealthPlugin()` (which mounts `/_/health`) can switch to the platform plugin in one step:

```go
// before
a.Use(server).Use(http.NewHealthPlugin())

// after
platformPlugin := platform.NewPlugin(platform.Config{Prefix: "/_"})  // keep /_ namespace
platformPlugin.RegisterOn(server)
a.Use(server).Use(platformPlugin)
```

`/_/health` becomes `/_/healthz`, and you also get `/_/livez`, `/_/readyz`, and `/_/version`. Drop the `Prefix` to migrate to plain root paths (`/healthz`, `/livez`, …) — the Kubernetes default.

`http.HealthPlugin` continues to work; nothing forces you to migrate. But don't mount both — they overlap on probe registration and you'll get inconsistent behaviour.

## Support and contract

The SDD owner is `go`. `go.putnami.dev/platform` is public, documented, maintained,
and classified `stable` in the workspace [support
catalog](../../../putnami.support.json). Before v1.0.0, a minor `0.x` release may
still contain a breaking change; the [release policy](../../../RELEASE.md) requires
release notes and migration documentation rather than strict compatibility between
every pre-1.0 minor.

The [platform endpoints specification](specs/platform-endpoints.json) defines the
contract, backed by [discover probes on the request path, validate their names at
start](doc/adr/0001-probe-discovery-happens-on-the-request-path.md).

Regression evidence covers [endpoint mounting, lifecycle state, probe discovery,
bounded probes, required probes, redaction, and protocol
conformance](plugin_test.go) and [version resolution and build-info
fallback](version_test.go).
