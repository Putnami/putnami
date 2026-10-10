# Platform endpoints

`go.putnami.dev/platform` mounts the standard operational HTTP surface every workload needs — liveness, readiness, version, and (optionally) pprof — and auto-discovers per-plugin probes from the application's module tree.

It replaces the per-workload boilerplate (and the legacy `http.NewHealthPlugin()`'s single `/_/health` endpoint) with one plugin you opt in to.

## Plugin setup

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

a := app.New("my-service").
    Use(server).
    Use(platformPlugin)
a.ListenAndServe()
```

`Use(platformPlugin)` is the only wiring step. When the application configures, the plugin mounts its endpoints on the application's single `http.ServerPlugin`, in any plugin order. An application that holds no server, or several, fails configure with an error that names `RegisterOn`. Call `platformPlugin.RegisterOn(server)` before the application configures to choose the server.

## Endpoints

| Path             | Purpose                                                                                                                                                              |
| ---------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `/livez`         | Lightweight liveness. Returns `200 {"status":"ok"}` whenever the handler can run. No probes, no flags. Safe to use as a Kubernetes liveness probe.                   |
| `/healthz`       | Liveness aggregate. `200` when running and every `app.HealthChecker` probe passes. `503 {"status":"unavailable"}` before `Start` and after `Stop`. `503 {"status":"degraded","checks":{…}}` when any probe fails. |
| `/readyz`        | Readiness aggregate. Same shape as `/healthz`, driven by `app.ReadinessChecker` probes, and `503 {"status":"unavailable"}` until the application completed startup: every `Starter` and module `OnStart` hook returned. The endpoint to use as a Kubernetes readiness probe. |
| `/version`       | Build metadata as JSON. Caller-supplied `VersionInfo` wins; empty fields fall back to `runtime/debug.ReadBuildInfo`.                                                 |
| `/debug/pprof/*` | `net/http/pprof` index, named profiles (`heap`, `goroutine`, `allocs`, …) and on-demand collectors (`profile`, `trace`, `cmdline`, `symbol`). Disabled by default.   |

Default mount is root. Set `Config.Prefix` to namespace (`Prefix: "/_"` → `/_/healthz`, …).

## Capability interfaces

The platform plugin discovers two interfaces by walking the module tree from the root and registers each implementation under its `Name()`.

```go
// in go.putnami.dev/app

type HealthChecker interface {
    Plugin
    CheckHealth(ctx context.Context) error
}

type ReadinessChecker interface {
    Plugin
    CheckReadiness(ctx context.Context) error
}
```

| Interface          | Semantics                                                          | Failure consequence (k8s)           |
| ------------------ | ------------------------------------------------------------------ | ----------------------------------- |
| `HealthChecker`    | Dependency is broken in a way only a restart fixes (liveness)      | Pod restart                         |
| `ReadinessChecker` | Dependency is temporarily down — drain traffic but don't restart   | Traffic drained, no restart         |

A single plugin can implement both: `CheckHealth` contributes to `/healthz`, `CheckReadiness` contributes to `/readyz`.

Implementations must be safe to call concurrently and respect context cancellation. The plugin enforces a per-request `Config.ProbeTimeout` (default `5s`) so a hung probe can't stall the endpoint.

**Built-in contributor:** `database.Plugin` implements `app.HealthChecker` — `pool.Ping` shows up under the `database` key on `/healthz` automatically when both plugins are mounted.

If a probe only needs DI-managed dependencies, contribute an injected function instead of resolving and storing the dependency in the plugin:

```go
func (p *Plugin) Configure(ctx context.Context, owner *app.Module) error {
    app.Contribute[app.ReadinessChecker](owner, app.HealthFunc("platform_configs", func(ctx context.Context, pools *psql.Pools) error {
        pool, err := pools.For("platform_configs")
        if err != nil {
            return err
        }
        return pool.Ping(ctx)
    }))
    return nil
}
```

## Explicit probes

For probes not owned by a plugin (an external URL, an ad-hoc check), register directly:

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

Explicit registrations win over auto-discovered probes of the same name — useful for overriding a plugin-provided probe in a specific environment.

## Configuration

```go
type Config struct {
    Prefix       string        // path prefix (default: "" → root)
    EnablePprof  bool          // expose /debug/pprof/* (default: false)
    Version      VersionInfo   // /version payload
    ProbeTimeout time.Duration // per-probe timeout (default: 5s)
}

type VersionInfo struct {
    Name      string `json:"name,omitempty"`
    Version   string `json:"version,omitempty"`
    SHA       string `json:"sha,omitempty"`
    Branch    string `json:"branch,omitempty"`
    BuildTime string `json:"buildTime,omitempty"`
}
```

### Build metadata

For production builds, embed version info at link time:

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

Build with `go build -ldflags "-X main.version=1.2.3 -X main.commit=$(git rev-parse HEAD)"`. Leaving `Version` empty falls back to `runtime/debug.ReadBuildInfo` (module path, `vcs.revision`, `vcs.time` when built with `-buildvcs`).

### pprof

`/debug/pprof/*` exposes Go's runtime profiler. Off by default — profiles reveal heap layout and goroutine details, and the CPU/trace collectors are expensive.

```go
platform.NewPlugin(platform.Config{EnablePprof: true})
```

Do not expose pprof on a public port. Mount the platform plugin on a separate admin server, or restrict access via a sidecar / network policy.

## Migrating from `http.NewHealthPlugin()`

The legacy `http.HealthPlugin` (single `/_/health` endpoint) still works and now also auto-discovers `app.HealthChecker` implementations. To get the full operational surface:

```go
// before
a.Use(server).Use(http.NewHealthPlugin())

// after
platformPlugin := platform.NewPlugin(platform.Config{Prefix: "/_"}) // keep /_ namespace
a.Use(server).Use(platformPlugin)
```

`/_/health` becomes `/_/healthz`, and you also get `/_/livez`, `/_/readyz`, `/_/version`. Drop the `Prefix` for plain k8s-style paths (`/healthz`, `/livez`, …). Both plugins can share one server: each discovers the same probes and reports them on its own route. The `go-server` starter composes both, because `putnami qualify` and deployment probes wait on `/readyz`.
## Support and contract

`go.putnami.dev/platform` is `stable` in the workspace support catalog. Its
behavior is defined by the platform endpoints specification and its accepted
decision record next to the package source (`go/framework/platform/specs/` and
`go/framework/platform/doc/adr/`). Before v1.0.0 a minor `0.x` release may still
contain a documented breaking change; strict compatibility between every pre-1.0
minor is not promised.
