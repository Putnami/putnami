# Plugins & Lifecycle

`go.putnami.dev/app` provides the application lifecycle, plugin architecture, and module composition system.

## Application

The application is the root module and lifecycle orchestrator:

```go
import "go.putnami.dev/app"

a := app.New("my-service")
a.Module.Use(httpServer)
a.Module.Use(healthPlugin)
a.ListenAndServe()
```

### Application methods

| Method | Description |
|--------|-------------|
| `New(name)` | Create a new application |
| `ProvideFunc(constructors...)` | Register constructor-based DI providers |
| `ProvideScopedFunc(constructors...)` | Register scoped constructor providers |
| `InvokeFunc(fns...)` | Run functions after DI is built |
| `Run(fn)` | Set a custom runner |
| `Start(ctx)` | Start the application lifecycle |
| `Stop(ctx)` | Graceful shutdown with a caller-supplied deadline |
| `ListenAndServe()` | Start + signal handling + Stop |
| `Context()` | Get the DI `ContainerContext` |
| `IsRunning()` | Check if running |
| `Validate()` | Build and validate without migrations or runtime start |
| `Describe(outputDir, targets)` | Emit build-time artifacts without runtime start |

### ListenAndServe

`ListenAndServe()` is the standard entry point. It calls `Start()`, waits for `SIGINT` or `SIGTERM`, then calls `Stop()`:

```go
func main() {
    a := app.New("my-service")
    a.Module.Use(server)

    if err := a.ListenAndServe(); err != nil {
        log.Fatal(err)
    }
}
```

### Custom runner

For applications that need a main loop:

```go
a.Run(func(ctx context.Context) error {
    // ctx is canceled on shutdown
    ticker := time.NewTicker(10 * time.Second)
    defer ticker.Stop()

    for {
        select {
        case <-ctx.Done():
            return nil
        case <-ticker.C:
            processJobs()
        }
    }
})
```

## Plugins

Every framework component (HTTP server, SQL pool, event broker, health checks) is a plugin. Plugins implement lifecycle interfaces.

### Plugin interface

The base interface requires only a name:

```go
type Plugin interface {
    Name() string
}
```

### Lifecycle interfaces

Implement one or more to participate in the application lifecycle:

```go
// Sequential — initialize resources, register routes
type Configurer interface {
    Plugin
    Configure(ctx context.Context, owner *Module) error
}

// Parallel — start servers, subscribe to events
type Starter interface {
    Plugin
    Start(ctx context.Context, owner *Module) error
}

// Once startup completed, before the ready record — must not block
type StartupObserver interface {
    Plugin
    StartupCompleted()
}

// The addresses a plugin bound while starting, carried by the ready record
type EndpointReporter interface {
    Plugin
    ReadyEndpoints() []runtime.ReadyEndpoint
}

// Reverse order — graceful shutdown
type Stopper interface {
    Plugin
    Stop(ctx context.Context, owner *Module) error
}

// Build-time — deterministic artifacts below the requested output directory
type Describer interface {
    Plugin
    Describe(ctx *DescribeContext) error
}
```

### Creating a plugin

```go
type MetricsPlugin struct {
    collector *MetricsCollector
}

func NewMetricsPlugin() *MetricsPlugin {
    return &MetricsPlugin{}
}

func (p *MetricsPlugin) Name() string { return "metrics" }

func (p *MetricsPlugin) Configure(ctx context.Context, owner *app.Module) error {
    p.collector = NewMetricsCollector()
    return nil
}

func (p *MetricsPlugin) Start(ctx context.Context, owner *app.Module) error {
    return p.collector.Start(ctx)
}

func (p *MetricsPlugin) Stop(ctx context.Context, owner *app.Module) error {
    return p.collector.Flush(ctx)
}
```

### Registering plugins

```go
a := app.New("my-service")
a.Module.Use(fhttp.NewServerPlugin(fhttp.ServerConfig{Port: 3000}))
a.Module.Use(fhttp.NewHealthPlugin())
a.Module.Use(NewMetricsPlugin())
```

A plugin that answers routes mounts itself. The health plugin above registers
`GET /_/health` on the application's single HTTP server during `Configure`, and
the platform plugin and the events plugin under push delivery follow the same
rule. An application that holds no server, or several, fails configure with an
error that names `RegisterOn`, the call that chooses the server.

## Lifecycle phases

The runtime lifecycle is phase-major: every module completes a phase before the
next phase begins.

```
1. Modules.PreConfigure  (top-down)    — before DI exists
2. Build DI                              — validate graph, resolve eager singletons
3. Plugins.Configure     (sequential)  — register routes and finalize wiring
4. Modules.PostConfigure (bottom-up)   — coordinate with DI available
5. Migrate                                — apply registered automatic migrations
6. Invoke                 (sequential)  — run InvokeFunc functions
7. Plugins.Start          (parallel)    — start servers and workers
8. Modules.OnStart        (top-down)    — after plugins are running
   Ready                                 — StartupObservers, then the ready record
9. Run / wait for cancellation
10. Modules.OnStop        (bottom-up)   — before plugins stop
11. Plugins.Stop          (reverse)     — graceful shutdown
12. Close DI                              — release constructed providers
```

### Phase details

**PreConfigure** — Module hooks run root-first before the container exists.

**Build DI** — Always creates the application container, including framework
services such as the migration registry. Runtime startup validates the graph and
resolves non-lazy singletons.

**Configure** — Plugins initialize in registration order with DI available.
This is where HTTP plugins register routes and handlers finalize their wiring.

**PostConfigure** — Module hooks run leaves-first after every plugin has
configured.

**Migrate** — Pending migrations run across registered kinds before invokers and
listeners.

**Invoke** — Functions registered with `InvokeFunc` run with DI-resolved parameters. Use this to wire routes or perform setup that requires resolved dependencies.

**Start** — All `Starter` plugins start in parallel and returned failures are
aggregated after every starter finishes. A phase timeout cancels siblings and
waits a bounded interval for cooperative starters to drain.

**Ready** — Once every `Starter` and every module `OnStart` hook returned
without an error, `Start` calls `StartupCompleted` on every `StartupObserver`,
then logs `🤖 ready`. That record carries a runtime-protocol `workload`
readiness claim under the reserved `putnami.ready` key, with the endpoints every
`EndpointReporter` bound (the HTTP server reports its listener). An endpoint
the runtime protocol rejects, such as port 0, is dropped with a warning rather
than voiding the claim. The platform
plugin is a `StartupObserver`: its `/readyz` answers ready only from then on. A
starter or hook that is still running delays both, and a failed or timed-out
start does neither.

**Stop** — Module `OnStop` hooks run leaves-first, then `Stopper` plugins run in
reverse registration order, and DI close hooks run last.

## Modules

Modules group plugins, DI providers, and sub-modules into composable units:

```go
// Create a module
auth := app.NewModule("auth")
auth.Path("/auth")
auth.Provide(inject.AutoProvide(NewAuthService))
auth.Use(authPlugin)

// Create another module
api := app.NewModule("api")
api.Path("/api")
api.Use(apiPlugin)

// Compose into the application
a := app.New("my-service")
a.Module.Use(server)
a.Module.Use(auth)
a.Module.Use(api)
```

### Module path prefix

Modules can define a path prefix. The HTTP server prepends the full module path to all routes registered during configure:

```go
api := app.NewModule("api")
api.Path("/api/v1")

// Routes registered in this module's plugins will be prefixed with /api/v1
// e.g., GET /users → GET /api/v1/users
```

The full path is computed from root to leaf:

```go
root := app.NewModule("root")
root.Path("/app")

child := app.NewModule("child")
child.Path("/api")

child.FullPath() // "/app/api" when mounted under root
```

### Module DI

Modules register providers whose constructor dependencies are validated when
the application container starts:

```go
auth := app.NewModule("auth")
auth.Provide(inject.AutoProvide(NewAuthService))
```

If `NewAuthService` requires `*Database` and no visible provider supplies it,
startup fails during DI validation.

### Boundary security

The `app` module owns composition and paths; it does not expose a `Secure`
method. Apply authentication and authorization through the HTTP/security
plugins at the route or middleware boundary. See
[Security](/docs/frameworks/go/security) for the supported APIs.

### Shutdown hooks

```go
a.Module.OnStop(func(ctx context.Context) error {
    fmt.Println("cleaning up...")
    return nil
})
```

### Introspection

```go
// Collect all plugins from the module tree
plugins := a.Module.CollectPlugins()

// Collect all modules (self + descendants)
modules := a.Module.CollectModules()

// Navigate or inspect composition state
root := child.Root()
path := child.FullPath()
container := child.Container() // available after DI build
```

## Constructor-based DI

The application provides a convenient fx-style DI API:

```go
a := app.New("my-service")

// Register constructors
a.ProvideFunc(
    NewDatabase,       // func(cfg *Config) (*Database, error)
    NewUserService,    // func(db *Database) *UserService
)

// Scoped constructors
a.ProvideScopedFunc(
    NewRequestContext, // func() *RequestContext — new per scope
)

// Invoke after DI is built
a.InvokeFunc(func(users *UserService, server *fhttp.ServerPlugin) {
    server.GET("/users", func(ctx *fhttp.Context) *fhttp.Response {
        return fhttp.JSON(users.List(ctx.Context()))
    })
})
```

Registering user providers is optional. The application container itself is
always created so framework-owned services remain available consistently.

## Support and compatibility

`go.putnami.dev/app` is a public, documented, maintained package classified
`stable`. Its lifecycle specification and accepted ADR live next to the package
source. Before v1.0.0, minor `0.x` releases may still contain documented
breaking changes; strict compatibility across every pre-1.0 minor is not
promised.

## Error codes

| Code | Description |
|------|-------------|
| `app.already_running` | Application is already running |
| `app.configure` | Plugin configure failed |
| `app.register` | DI registration failed |
| `app.start` | Plugin start failed |
| `app.stop` | Plugin stop failed |
| `app.invoke` | Invoke function failed |
| `app.runner` | Custom runner failed |
| `app.health` | Health/readiness probe wiring or resolution failed |

## Related guides

- [Dependency Injection](/docs/frameworks/go/dependency-injection) — DI container details
- [HTTP & Middleware](/docs/frameworks/go/http) — HTTP server plugin
- [Configuration](/docs/frameworks/go/configuration) — config loading
