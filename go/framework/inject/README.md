# Dependency Injection

The `inject` package provides an **optional** dependency injection container. DI is available for teams that want it, but the framework works without it — you can wire dependencies explicitly in `main()`.

Two API styles are supported and can be mixed freely:

- **Constructor-based** (fx-style) — auto-wires by type, minimal boilerplate
- **Token-based** (explicit) — full control with named tokens and visibility

## Constructor-Based DI (fx-style)

Inspired by Uber's [fx](https://pkg.go.dev/go.uber.org/fx), this API uses Go functions as constructors. Input parameters are dependencies (resolved by type), return values are what gets registered.

### AutoProvide

```go
// Constructor: deps are function params, registered type is the return
inject.AutoProvide(NewUserService)  // func(db *sql.DB) *UserService

// Supported signatures:
//   func() T
//   func() (T, error)
//   func(dep1 A, dep2 B) T
//   func(dep1 A, dep2 B) (T, error)
```

### AutoProvideScoped

Creates a new instance per scope (e.g., per HTTP request):

```go
inject.AutoProvideScoped(NewUserRepository)  // func(db *sql.DB) *UserRepository
```

### AutoProvideNamed

Registers under a name (for multiple providers of the same type):

```go
inject.AutoProvideNamed("primary", NewPrimaryDB)   // func(cfg Config) *sql.DB
inject.AutoProvideNamed("readonly", NewReadonlyDB)  // func(cfg Config) *sql.DB
```

### ProvideInstance

Registers a pre-built value by its type:

```go
cfg := &AppConfig{Port: 8080}
inject.ProvideInstance(cfg)  // registered as *AppConfig
```

### App-Level API

The `app` package provides convenience methods:

```go
a := app.New("my-service")

// Register constructors (variadic)
a.ProvideFunc(LoadConfig, NewDB, NewUserService)

// Register scoped constructors
a.ProvideScopedFunc(NewRequestLogger, NewUserRepository)

// Run functions after DI is built (params auto-resolved)
a.InvokeFunc(func(server *http.ServerPlugin, users *UserService) {
    server.GET("/users", listUsersHandler(users))
})
```

## Token-Based DI (explicit)

For cases where you need named tokens, visibility control, or explicit dependency declarations.

### Tokens

Tokens identify dependencies in the container:

```go
// Class token — identified by Go type
token := inject.TokenOf[*UserService]()

// Named token — identified by name + type
dbToken := inject.Named[*sql.DB]("primary")
cacheToken := inject.Named[*sql.DB]("cache")

// Tag selector — for multi-resolution
plugins := inject.Tagged[Plugin]("http")
```

### Providers

Register providers using `Provide` or `ProvideValue`:

```go
// Factory-based provider
inject.Provide(token, func(r inject.Resolver) (any, error) {
    db, err := inject.ResolveAs[*sql.DB](r, dbToken)
    if err != nil { return nil, err }
    return NewUserService(db), nil
}, inject.WithDeps(dbToken))

// Value provider
inject.ProvideValue(token, existingInstance)
```

### Provider Options

| Option | Description |
|--------|-------------|
| `WithScope(Scoped)` | New instance per scope (default: `Singleton`) |
| `WithVisibility(Private)` | Not visible to child containers (default: `Public`) |
| `WithTags("tag1", "tag2")` | Enable multi-resolution via `List()` |
| `WithDeps(token1, token2)` | Declare dependencies for validation |
| `WithOnClose(func() error)` | Cleanup hook called on `Close()` |
| `WithLazy()` | Don't resolve at startup |

Options work with both `AutoProvide` and `Provide`:

```go
inject.AutoProvide(NewDB,
    inject.WithOnClose(func() error { return db.Close() }),
    inject.WithTags("database"),
)
```

## Container Hierarchy

Containers form a parent-child tree. Children inherit parent providers (respecting visibility):

```go
root := inject.NewContainer("root", nil)
child := root.CreateChild("module-a")

// Register in root — visible to all children
root.Register(inject.ProvideValue(dbToken, db))

// Register in child — only visible within child
child.Register(inject.Provide(svcToken, factory, inject.WithVisibility(inject.Private)))
```

## ContainerContext

`ContainerContext` is the application-level entry point:

```go
cc := inject.NewContainerContext("app")
cc.Register(inject.ProvideValue(dbToken, db))
cc.Register(inject.AutoProvide(NewUserService))

// Optionally declare tokens that must be provided before Start() succeeds
cc.Require(svcToken)

// Start — validates and resolves singletons
cc.Start()
defer cc.Close()

// Resolve
val, err := cc.Get(svcToken)
```

## Scoped Resolution

Scoped providers create new instances per scope (e.g., per HTTP request):

```go
cc.Register(inject.AutoProvideScoped(NewRequestLogger))

// Run in a scope
cc.Scope(ctx, func(ctx context.Context, scope inject.ScopeContext) error {
    val, _ := scope.Get(requestToken) // New instance for this scope
    // Or use the context-based helper:
    val2, _ := inject.Resolve[*RequestData](ctx, requestToken)
    return nil
})
```

## Testing with Fork

Fork a container context to override providers in tests:

```go
forked, _ := cc.Fork().
    OverrideValue(dbToken, mockDB).
    Start()
defer forked.Close()
```

## Validation

The container validates at startup:
- **Missing dependencies** — tokens referenced but not registered
- **Circular dependencies** — DFS-based cycle detection
- **Scope violations** — singleton depending on scoped; resolve the scoped value from the request context instead

## Support and contract

The SDD owner is `go`. `go.putnami.dev/inject` is public, documented, maintained, and classified
`stable` in the workspace [support catalog](../../../putnami.support.json).
Before v1.0.0, a minor `0.x` release may still contain a breaking change; the
[release policy](../../../RELEASE.md) requires release notes and migration
documentation rather than strict compatibility between every pre-1.0 minor.

The [dependency injection specification](specs/dependency-injection.json) and
[lifetime ADR](doc/adr/0001-lifetimes-own-construction-and-cleanup.md) define the
contract. Regression evidence covers [lifetimes and transactional
startup](lifetime_test.go), [concurrent resolution](concurrent_test.go), [scoped
cleanup](scope_finalizer_test.go), and [field wiring](wire_test.go).
