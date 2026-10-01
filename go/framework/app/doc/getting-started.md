# Application Lifecycle and Plugin System

`go.putnami.dev/app` provides the application lifecycle, module system, and plugin architecture for the Putnami Go framework. Applications are composed of modules, which contain plugins that participate in ordered lifecycle phases.

## Creating an Application

An application is the root module and lifecycle orchestrator. Create one with `app.New`:

```go
package main

import (
    "go.putnami.dev/app"
)

func main() {
    a := app.New("my-service")
    if err := a.ListenAndServe(); err != nil {
        panic(err)
    }
}
```

`ListenAndServe` starts the application and blocks until a `SIGINT` or `SIGTERM` signal is received, then performs graceful shutdown.

For more control over the context, use `Start` and `Stop` directly:

```go
ctx, cancel := context.WithCancel(context.Background())
defer cancel()

if err := a.Start(ctx); err != nil {
    log.Fatal(err)
}
// Pass a context with a deadline to bound graceful shutdown; Background is unbounded.
defer a.Stop(context.Background())
```

## Lifecycle Phases

The lifecycle is phase-major: each phase completes across the whole module tree
before the next begins. Module hooks bracket the plugin phases, and every
lifecycle method and hook takes a `context.Context`.

| Phase | Interface / Hook | Execution | Purpose |
|-------|------------------|-----------|---------|
| **Modules.PreConfigure** | `OnPreConfigure` | top-down | Pre-DI setup; `Module.Container()` is nil here |
| **DI Build** | (automatic) | - | Build DI container, set `Module.Container()`, auto-wire plugin fields |
| **Plugins.Configure** | `Configurer` | sequential | Register routes, finalize DI bindings (DI available) |
| **Modules.PostConfigure** | `OnPostConfigure` | bottom-up | Coordinate a module's plugins once all are wired |
| **Migrate** | (automatic) | sequential | Apply pending migrations across every registered kind |
| **Invoke** | `InvokeFunc` | sequential | Run DI-resolved setup callbacks |
| **Plugins.Start** | `Starter` | parallel | Start servers, subscribe to events, open connections |
| **Modules.OnStart** | `OnStart` | top-down | Startup that depends on a module's plugins running |
| **Modules.OnStop** | `OnStop` | bottom-up | Cleanup before the module's plugins stop |
| **Plugins.Stop** | `Stopper` | reverse | Graceful shutdown, close connections, flush buffers |

There is also a build-time phase:

| Phase | Interface | Purpose |
|-------|-----------|---------|
| **Describe** | `Describer` | Emit committed build-time artifacts (proto, OpenAPI, …), then exit before Start |

## Plugins

A plugin participates in the application lifecycle by implementing the `Plugin` interface plus any combination of lifecycle interfaces. Only implement the phases your plugin needs.

```go
// Plugin is the base interface. Every plugin must have a name.
type Plugin interface {
    Name() string
}
```

### Lifecycle Interfaces

```go
// Configurer runs during the configure phase (after DI is built).
type Configurer interface {
    Plugin
    Configure(ctx context.Context, owner *Module) error
}

// Starter runs during the start phase (after DI is built).
type Starter interface {
    Plugin
    Start(ctx context.Context, owner *Module) error
}

// Stopper runs during the shutdown phase.
type Stopper interface {
    Plugin
    Stop(ctx context.Context, owner *Module) error
}

// Describer runs during the build-time describe phase to emit artifacts.
type Describer interface {
    Plugin
    Describe(ctx *DescribeContext) error
}
```

### Writing a Plugin

A plugin only needs to implement the phases it participates in:

```go
type MetricsPlugin struct {
    server *http.Server
}

func (p *MetricsPlugin) Name() string { return "metrics" }

func (p *MetricsPlugin) Start(ctx context.Context, owner *app.Module) error {
    p.server = &http.Server{Addr: ":9090"}
    go p.server.ListenAndServe()
    return nil
}

func (p *MetricsPlugin) Stop(ctx context.Context, owner *app.Module) error {
    return p.server.Shutdown(ctx)
}
```

### Registering Plugins

Add plugins to the application (or any module) with `Use`:

```go
a := app.New("my-service")
a.Use(&MetricsPlugin{})
```

## Modules

Modules group plugins, DI providers, and sub-modules into composable units. They form a tree: each module may contain child modules and plugins.

```go
usersModule := app.NewModule("users").
    Path("/users").
    Use(&UsersPlugin{})

adminModule := app.NewModule("admin").
    Path("/admin").
    Use(&AdminPlugin{})

a := app.New("my-service")
a.Use(usersModule)
a.Use(adminModule)
```

### Path Prefixes

Modules can declare a base path. Child module paths are concatenated with their parent:

```go
api := app.NewModule("api").Path("/api")
v1 := app.NewModule("v1").Path("/v1")
api.Use(v1)

// v1.FullPath() returns "/api/v1"
```

### Authorization

Access control is not part of `go.putnami.dev/app`. Apply authorization with the
dedicated `go.putnami.dev/security` module (roles, scopes, guards, and identity
resolution) on the routes or handlers that need it.

### Shutdown Hooks

Register cleanup functions that run during application shutdown:

```go
m := app.NewModule("cache")
m.OnStop(func(ctx context.Context) error {
    return cache.Flush()
})
```

Shutdown hooks are called in reverse registration order, before plugin `Stop` methods.

## Dependency Injection

DI is optional: you never have to register a provider. The framework still always builds a DI container (it registers a per-app `*migration.Registry`), so `a.Context()` is non-nil between `Start` and `Stop`. The app module supports two DI styles that can be mixed freely.

### Token-Based DI

Register values or factories with explicit tokens using `Provide`:

```go
token := inject.Named[string]("app-name")
a.Provide(inject.ProvideValue(token, "my-service"))
```

Retrieve values from the container after startup:

```go
a.Run(func(ctx context.Context) error {
    val, _ := a.Context().Get(token)
    fmt.Println(val.(string)) // "my-service"
    return nil
})
```

### Constructor-Based DI (fx-style)

Register constructors whose parameters are resolved by type from the container:

```go
type DB struct { DSN string }
type UserService struct { DB *DB }

func NewDB() *DB {
    return &DB{DSN: "localhost:5432"}
}

func NewUserService(db *DB) *UserService {
    return &UserService{DB: db}
}

a := app.New("my-service")
a.ProvideFunc(NewDB, NewUserService)
```

Supported constructor signatures:

- `func() T`
- `func() (T, error)`
- `func(dep1 A, dep2 B) T`
- `func(dep1 A, dep2 B) (T, error)`

### Scoped Providers

Use `ProvideScopedFunc` for instances that should be created per scope (e.g., per HTTP request):

```go
a.ProvideScopedFunc(NewRequestLogger)
```

### Invokers

`InvokeFunc` registers functions called after the DI container is built but before plugins start. Parameters are resolved from the container. Use invokers for side effects like route registration:

```go
a.ProvideFunc(NewDB, NewUserService)
a.InvokeFunc(func(users *UserService) {
    fmt.Println("UserService ready:", users)
})
```

If an invoker returns an error, startup is aborted:

```go
a.InvokeFunc(func(db *DB) error {
    return db.Ping()
})
```

### Mixing DI Styles

Token-based and constructor-based DI can be combined in the same application:

```go
a := app.New("mixed")

// Token-based
nameToken := inject.Named[string]("app-name")
a.Provide(inject.ProvideValue(nameToken, "my-service"))

// Constructor-based
a.ProvideFunc(NewDB)
```

### Module-Level DI

Each module can register its own providers. All registrations are collected into a single container at startup:

```go
usersModule := app.NewModule("users").
    Provide(inject.ProvideValue(someToken, someValue))

a := app.New("my-service")
a.Use(usersModule)
a.ProvideFunc(NewDB)
```

## Custom Runner

The `Run` method sets a function that executes after all plugins start. The runner receives a context that is canceled on shutdown:

```go
a := app.New("worker")
a.Run(func(ctx context.Context) error {
    for {
        select {
        case <-ctx.Done():
            return nil
        case <-time.After(5 * time.Second):
            fmt.Println("tick")
        }
    }
})
```

Without a runner, the application blocks until the context is canceled (e.g., by a shutdown signal).

## Error Codes

The app module uses typed error codes for lifecycle failures:

| Code | Meaning |
|------|---------|
| `app.already_running` | `Start` called on an already-running application |
| `app.configure` | A plugin's `Configure` method failed |
| `app.register` | A DI registration failed |
| `app.start` | A plugin's `Start` method failed |
| `app.stop` | A plugin's `Stop` method or container close failed |
| `app.invoke` | An invoker function failed or received bad arguments |
| `app.runner` | The custom runner function returned an error |

## Complete Example

```go
package main

import (
    "context"
    "fmt"

    "go.putnami.dev/app"
    "go.putnami.dev/inject"
)

type DB struct{ DSN string }

func NewDB() *DB { return &DB{DSN: "localhost:5432"} }

type HealthPlugin struct{}

func (p *HealthPlugin) Name() string { return "health" }
func (p *HealthPlugin) Configure(ctx context.Context, owner *app.Module) error {
    fmt.Println("health: registering /healthz")
    return nil
}
func (p *HealthPlugin) Start(ctx context.Context, owner *app.Module) error {
    fmt.Println("health: ready")
    return nil
}
func (p *HealthPlugin) Stop(ctx context.Context, owner *app.Module) error {
    fmt.Println("health: stopped")
    return nil
}

func main() {
    a := app.New("my-service")

    // Register plugins
    a.Use(&HealthPlugin{})

    // Register DI providers
    a.ProvideFunc(NewDB)

    // Run invokers after DI is built
    a.InvokeFunc(func(db *DB) {
        fmt.Println("connected to", db.DSN)
    })

    // Compose modules
    api := app.NewModule("api").Path("/api")
    admin := app.NewModule("admin").Path("/admin")
    api.Use(admin)
    a.Use(api)

    // Block until shutdown signal
    if err := a.ListenAndServe(); err != nil {
        panic(err)
    }
}
```

## Best Practices

- **Implement only the lifecycle interfaces you need.** A plugin that only needs startup and shutdown should implement `Starter` and `Stopper`, not `Configurer`.
- **Use configure for registration, start for execution.** Register routes and resources in `Configure` (sequential, predictable order). Start servers and background goroutines in `Start` (parallel).
- **Keep plugins focused.** Each plugin should own a single concern (metrics, health checks, database connection).
- **Use modules for organization.** Group related plugins and DI providers into modules with path prefixes and lifecycle ownership. Keep authorization in the dedicated HTTP and security plugins.
- **Prefer `ProvideFunc` for type-safe DI.** Constructor-based DI avoids manual token management and catches wiring errors at startup.
- **Use `InvokeFunc` for side effects.** Route registration, event subscriptions, and other setup that depends on DI should go in invokers.
- **Register shutdown hooks for resources that need cleanup.** Use `OnStop` on the owning module, or implement `Stopper` on the plugin.
- **DI is optional.** Simple applications with no dependencies need not register any providers or invokers. The container itself is always built (registering a per-app `*migration.Registry`), so `a.Context()` is non-nil between `Start` and `Stop`.

## Contract and compatibility

See the [application lifecycle specification](../specs/application-lifecycle.json),
[phase-major lifecycle ADR](adr/0001-phase-major-lifecycle.md), and [support
evidence](../README.md#support-and-contract). The package is stable and
maintained, with the workspace's [pre-1.0 migration
policy](../../../../RELEASE.md), not strict compatibility between every `0.x`
minor.
