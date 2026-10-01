# Application Lifecycle

The `app` package provides the plugin-based application lifecycle and module composition system.

## Plugins

Plugins are the building blocks of a Putnami application. A plugin must implement `Name() string` and may optionally implement any lifecycle interface:

```go
type Plugin interface { Name() string }
type Configurer interface { Configure(context.Context, *Module) error }
type Starter   interface { Start(context.Context, *Module) error }
type Stopper   interface { Stop(context.Context, *Module) error }
type Describer interface { Describe(*DescribeContext) error }
```

Runtime phases run in order: **PreConfigure -> DI Build -> Configure ->
PostConfigure -> Migrate -> Invoke -> Start -> OnStart**. On shutdown:
**OnStop -> Stop -> DI close**. Describe mode has its own build-time contract and
does not start runtime plugins.

## Modules

Modules group plugins, DI providers, and sub-modules into a composable tree:

```go
users := app.NewModule("users").
    Path("/users").
    Provide(inject.ProvideValue(repoToken, userRepo)).
    Use(userPlugin)

admin := app.NewModule("admin").
    Path("/admin").
    Use(adminPlugin)

root := app.NewModule("root").
    Path("/api").
    Use(users).
    Use(admin)
```

Path prefixes concatenate through the tree — `admin` resolves to `/api/admin`.

## Application

`Application` is the root module plus a lifecycle orchestrator. Applications
may use explicit wiring, but the lifecycle always builds a container because it
owns framework services such as the migration registry.

### Without DI (explicit wiring)

```go
a := app.New("my-service")

// Add plugins directly
a.Module.Use(httpServer)
a.Module.Use(healthCheck)

a.Run(func(ctx context.Context) error {
    <-ctx.Done()
    return nil
})

a.ListenAndServe()
```

### With DI (constructor-based, fx-style)

```go
a := app.New("my-service")

// Register constructors — types are resolved automatically
a.ProvideFunc(LoadConfig, NewDB, NewUserService)

// Register scoped constructors (new instance per HTTP request)
a.ProvideScopedFunc(NewUserRepository)

// Run functions after DI is built
a.InvokeFunc(func(server *http.ServerPlugin, users *UserService) {
    server.GET("/users", listUsersHandler(users))
})

a.ListenAndServe()
```

### With DI (token-based, explicit)

```go
a := app.New("my-service")

// Register DI providers with explicit tokens
a.Provide(inject.ProvideValue(dbToken, db))
a.Provide(inject.Provide(svcToken, factory, inject.WithDeps(dbToken)))

// Add plugins and sub-modules
a.Module.Use(httpServer)
a.Module.Use(users)

a.Run(func(ctx context.Context) error {
    <-ctx.Done()
    return nil
})
a.ListenAndServe()
```

## Startup Phases

`ListenAndServe()` / `Start()` runs phase-major — each phase completes across
the whole module tree before the next begins:

1. Collect all plugins and registrations from the module tree
2. **Modules.PreConfigure** (top-down: root → leaves) — before DI exists
3. **Build DI** — validates and resolves singletons, sets `Module.Container()`, auto-wires plugin fields
4. **Plugins.Configure** (sequential) — register routes, finalize DI-injected handlers (DI available)
5. **Modules.PostConfigure** (bottom-up: leaves → root) — DI container ready
6. **Migrate** — apply pending migrations across every registered kind
7. **Invoke** — run `InvokeFunc` functions with DI-resolved params
8. **Plugins.Start** (parallel) — start servers, subscribe to events
9. **Modules.OnStart** (top-down) — after their plugins are running
10. Execute the runner, or wait for application cancellation; `ListenAndServe`
    owns SIGINT/SIGTERM cancellation

On shutdown:

11. **Modules.OnStop** (bottom-up reverse) — before their plugins stop
12. **Plugins.Stop** (reverse registration order) — graceful shutdown
13. **DI close** — close constructed providers and detach module containers

## Module Lifecycle

Beyond holding plugins, a module has its own lifecycle hooks that the app
drives, phase-major, around the plugin phases:

```go
m.OnPreConfigure(func(ctx context.Context) error { return nil })                              // top-down, before DI build
m.OnPostConfigure(func(ctx context.Context, cc *inject.ContainerContext) error { return nil }) // bottom-up, DI ready
m.OnStart(func(ctx context.Context) error { return nil })                                     // top-down, after plugins start
m.OnStop(func(ctx context.Context) error { return db.Close() })                               // bottom-up, before plugins stop
```

Phase order:

```
1. Modules.PreConfigure   (top-down: root → leaves)
2. DI graph build
3. Plugins.Configure
4. Modules.PostConfigure  (bottom-up: leaves → root)
5. Migrate
6. Invoke
7. Plugins.Start
8. Modules.OnStart        (top-down, after their plugins)
   ─── running ───
9. Modules.OnStop         (bottom-up reverse)
10. Plugins.Stop          (reverse)
11. DI close
```

`OnStop` hooks run *before* the module's plugins stop, leaves-first.

## Support and contract

The SDD owner is `go`. `go.putnami.dev/app` is public, documented, maintained, and classified
`stable` in the workspace [support catalog](../../../putnami.support.json).
Before v1.0.0, a minor `0.x` release may still contain a breaking change; the
[release policy](../../../RELEASE.md) requires release notes and migration
documentation rather than strict compatibility between every pre-1.0 minor.

The behavior above is pinned by the [application lifecycle
specification](specs/application-lifecycle.json) and its [phase-major lifecycle
ADR](doc/adr/0001-phase-major-lifecycle.md). Regression evidence includes the
[module lifecycle](module_lifecycle_test.go), [start failure](start_errors_test.go),
[start timeout](start_timeout_test.go), and [describe](describe_test.go) tests.
