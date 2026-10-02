# Dependency Injection

`go.putnami.dev/inject` provides a hierarchical DI container with generics, named tokens, scoped providers, and cycle detection. DI is entirely optional — you can wire services explicitly and skip the container altogether.

## Tokens

Tokens identify dependencies in the container. There are three kinds:

### Class tokens

Derived from Go types. Most common when using constructor-based DI:

```go
import "go.putnami.dev/inject"

token := inject.TokenOf[*UserService]()
```

### Named tokens

For non-struct values or when you need multiple instances of the same type:

```go
apiURL := inject.Named[string]("api-url")
dbPool := inject.Named[*sql.Pool]("primary-db")
```

### Tag selectors

For resolving multiple providers at once:

```go
plugins := inject.Tagged[Plugin]("plugin")
```

## Provider registration

### Factory provider

```go
reg := inject.Provide(
    inject.TokenOf[*UserService](),
    func(r inject.Resolver) (any, error) {
        db, err := inject.ResolveAs[*Database](r, inject.TokenOf[*Database]())
        if err != nil {
            return nil, err
        }
        return NewUserService(db), nil
    },
)
```

### Value provider

```go
reg := inject.ProvideValue(inject.Named[string]("api-url"), "https://api.example.com")
```

### Instance provider

```go
reg := inject.ProvideInstance(myLogger)
```

### Auto-provide (constructor-based)

The most concise registration. Constructor parameters are resolved by type, and the return value is registered by type:

```go
// func NewUserService(db *Database) *UserService
reg := inject.AutoProvide(NewUserService)
```

Supported constructor signatures:

```go
func() T
func() (T, error)
func(dep1 A, dep2 B) T
func(dep1 A, dep2 B) (T, error)
```

### Auto-provide variants

```go
// Scoped — new instance per scope (e.g., per HTTP request)
inject.AutoProvideScoped(NewRequestContext)

// Named — register under a custom name
inject.AutoProvideNamed("primary-db", NewDatabase)
```

## Provider options

Options configure provider behavior:

```go
inject.Provide(token, factory,
    inject.WithScope(inject.Scoped),
    inject.WithVisibility(inject.Private),
    inject.WithTags("plugin", "auth"),
    inject.WithDeps(inject.TokenOf[*Database]()),
    inject.WithOnClose(func() error { return db.Close() }),
    inject.WithLazy(),
    inject.WithDynamic(),
)
```

| Option | Default | Description |
|--------|---------|-------------|
| `WithScope(Scoped)` | `Singleton` | New instance per scope instead of one global instance |
| `WithVisibility(Private)` | `Public` | Hide from child containers |
| `WithTags(...)` | — | Tags for multi-resolution via `List()` |
| `WithDeps(...)` | — | Explicit dependency declarations for validation |
| `WithOnClose(fn)` | — | Cleanup hook called on `Close()` |
| `WithLazy()` | — | Skip resolution at startup; resolve on first access |
| `WithDynamic()` | — | Allow refresh after startup |

## Container

### Creating a container

```go
root := inject.NewContainer("root", nil)
root.Register(inject.AutoProvide(NewDatabase))
root.Register(inject.AutoProvide(NewUserService))
```

### Resolving dependencies

```go
val, err := root.Get(inject.TokenOf[*UserService]())
userService := val.(*UserService)

// Check existence
if root.Has(inject.TokenOf[*UserService]()) {
    // registered
}
```

### Hierarchy

Child containers inherit parent registrations. Private providers are not visible to children:

```go
child := root.CreateChild("auth-module")
child.Register(inject.Provide(
    inject.TokenOf[*TokenValidator](),
    factory,
    inject.WithVisibility(inject.Private), // only visible in this child
))
```

### Validation

```go
issues := root.Validate()
for _, err := range issues {
    fmt.Println(err) // missing deps, circular deps, scope violations
}
```

## ContainerContext

`ContainerContext` is the application-level entry point that adds lifecycle management and scoping on top of `Container`:

```go
cc := inject.NewContainerContext("app")
cc.Register(inject.AutoProvide(NewDatabase))
cc.Register(inject.AutoProvide(NewUserService))

// Start — validates graph, resolves non-lazy singletons
if err := cc.Start(); err != nil {
    log.Fatal(err)
}

// Resolve
val, _ := cc.Get(inject.TokenOf[*UserService]())

// Close — runs onClose hooks in reverse order
defer cc.Close()
```

### Modules

Mount a group of registrations as a child container with explicit requirements:

```go
authRegs := []inject.Registration{
    inject.AutoProvide(NewAuthService),
    inject.AutoProvide(NewTokenValidator),
}
authReqs := []inject.Token{
    inject.TokenOf[*Database](), // must be provided by parent
}

cc.Mount("auth", authRegs, authReqs)
```

## Scopes

### Singleton (default)

One instance shared for the container's lifetime:

```go
inject.AutoProvide(NewDatabase) // singleton by default
```

### Scoped

New instance for each scope (e.g., per HTTP request):

```go
inject.AutoProvideScoped(NewRequestContext)
```

### Using scoped services

```go
err := cc.Scope(ctx, func(ctx context.Context, scope inject.ScopeContext) error {
    reqCtx, _ := scope.Get(inject.TokenOf[*RequestContext]())
    // reqCtx is unique to this scope

    // Same scope = same instance
    reqCtx2, _ := scope.Get(inject.TokenOf[*RequestContext]())
    // reqCtx == reqCtx2
    return nil
})
```

### Context-based resolution

Inside a scope, resolve from `context.Context` directly:

```go
err := cc.Scope(ctx, func(ctx context.Context, scope inject.ScopeContext) error {
    // Typed resolution from context
    userService, err := inject.Resolve[*UserService](ctx, inject.TokenOf[*UserService]())
    if err != nil {
        return err
    }
    return userService.Process(ctx)
})
```

### Detached scopes

For scopes with manual lifetime control:

```go
scope, err := cc.CreateScope()
if err != nil {
    return err
}
defer scope.Close()

// Use the scope's context
scopedCtx := scope.Context(ctx)
userService, _ := inject.Resolve[*UserService](scopedCtx, inject.TokenOf[*UserService]())
```

## Tags and multi-resolution

Tag providers for grouped resolution:

```go
cc.Register(inject.AutoProvide(NewPluginA, inject.WithTags("plugin")))
cc.Register(inject.AutoProvide(NewPluginB, inject.WithTags("plugin")))

// Resolve all tagged providers
vals, _ := cc.List(inject.Tagged[Plugin]("plugin"))
// vals contains [PluginA, PluginB]
```

Inside factories, use the resolver:

```go
inject.Provide(
    inject.TokenOf[*PluginManager](),
    func(r inject.Resolver) (any, error) {
        plugins, _ := r.ResolveAll(inject.Tagged[Plugin]("plugin"))
        return NewPluginManager(plugins), nil
    },
)
```

## Fx-style DI with Application

The `app.Application` provides a higher-level API for constructor-based DI:

```go
import "go.putnami.dev/app"

a := app.New("my-service")

// Register constructors — params resolved by type
a.ProvideFunc(
    NewDatabase,     // func() (*Database, error)
    NewUserService,  // func(db *Database) *UserService
    NewUserHandler,  // func(users *UserService) *UserHandler
)

// Scoped constructors — new instance per scope
a.ProvideScopedFunc(NewRequestContext)

// Invoke — run after DI is built
a.InvokeFunc(func(handler *UserHandler, server *fhttp.ServerPlugin) {
    server.GET("/users", handler.List)
})

a.ListenAndServe()
```

## Scoped access from a singleton

A singleton must never depend directly on a scoped provider. Instead, pass the
request context explicitly and resolve the scoped value at the point of use:

```go
// RequestID is scoped — new per request
// AuditLogger is singleton — constructed once

func (l *AuditLogger) Log(ctx context.Context, msg string) error {
    id, err := inject.Resolve[*RequestID](ctx, inject.TokenOf[*RequestID]())
    if err != nil {
        return err
    }
    // use id
    return nil
}
```

## Testing

### Fork and override

Fork a `ContainerContext` for test isolation:

```go
func TestUserService(t *testing.T) {
    fork := cc.Fork()
    fork.Override(inject.TokenOf[*Database](), func(r inject.Resolver) (any, error) {
        return NewMockDatabase(), nil
    })
    fork.OverrideValue(inject.TokenOf[*EmailService](), &MockEmailService{})

    testCC, err := fork.Start()
    if err != nil {
        t.Fatal(err)
    }
    defer testCC.Close()

    val, _ := testCC.Get(inject.TokenOf[*UserService]())
    userService := val.(*UserService)
    // test with mocked dependencies
}
```

### Manual test container

```go
func TestUserService(t *testing.T) {
    cc := inject.NewContainerContext("test")
    cc.Register(inject.ProvideValue(inject.TokenOf[*Database](), mockDB))
    cc.Register(inject.AutoProvide(NewUserService))

    if err := cc.Start(); err != nil {
        t.Fatal(err)
    }
    defer cc.Close()

    val, _ := cc.Get(inject.TokenOf[*UserService]())
    userService := val.(*UserService)
    // test
}
```

## Validation

The container validates the entire dependency graph during `Start()`:

- **Missing dependencies** — all declared deps must be registered
- **Circular dependencies** — detected via DFS-based graph analysis
- **Scope violations** — singleton depending on scoped (see [Scoped access from a singleton](#scoped-access-from-a-singleton))
- **Requirement validation** — mounted modules' requirements must be satisfied

```go
cc.Register(inject.AutoProvide(NewA))  // depends on B
cc.Register(inject.AutoProvide(NewB))  // depends on A

err := cc.Start()
// Error: circular dependency: *A → *B → *A
```

## Error codes

| Code | Description |
|------|-------------|
| `inject.not_registered` | Token not found in container or parents |
| `inject.circular_dependency` | Circular dependency detected |
| `inject.scope_violation` | Singleton depends on scoped |
| `inject.container_closed` | Operation on a closed container |
| `inject.duplicate_provider` | Token already registered |
| `inject.requirement_not_met` | Module requirement not satisfied |
| `inject.type_mismatch` | Resolved value doesn't match expected type |
| `inject.factory_failed` | Factory function returned an error |

## Related guides

- [Plugins & Lifecycle](/docs/frameworks/go/plugins-and-lifecycle) — application-level DI
- [Configuration](/docs/frameworks/go/configuration) — config-to-DI bridge
- [Testing](/docs/frameworks/go/testing) — testing with DI

## Support and compatibility

`go.putnami.dev/inject` is a public, documented, maintained package classified
`stable`. Its dependency-injection specification and accepted lifetime ADR live
next to the package source. Before v1.0.0, minor `0.x` releases may still
contain documented breaking changes; strict compatibility across every pre-1.0
minor is not promised.
