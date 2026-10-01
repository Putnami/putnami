# Dependency Injection

`go.putnami.dev/inject` is a hierarchical dependency injection container for Go. It supports constructor-based auto-wiring, named and tagged tokens, scoped lifetimes, parent-child container hierarchies with visibility control, circular dependency detection, and lifecycle management. It uses only the Go standard library and `go.putnami.dev/errors`.

## Creating a Container

Use `NewContainerContext` for application-level DI. It manages registration, validation, startup, and shutdown.

```go
import "go.putnami.dev/inject"

cc := inject.NewContainerContext("app")

// Register providers (before Start)
cc.Register(inject.AutoProvide(NewConfig))
cc.Register(inject.AutoProvide(NewDatabase))
cc.Register(inject.AutoProvide(NewUserService))

// Start validates the graph and resolves all non-lazy singletons.
// Register Close *before* the error check: a failed Start already rolls back
// (disposes any singletons it built, resets to not-started), so Close is a safe
// no-op on the failure path and still releases singletons built by a later
// successful retry.
defer cc.Close()
if err := cc.Start(); err != nil {
    log.Fatal(err)
}
```

For lower-level control, use `NewContainer` directly:

```go
c := inject.NewContainer("root", nil) // nil parent = root container
c.Register(inject.ProvideValue(portToken, 8080))
```

## Registering Services

### Constructor-Based (AutoProvide)

The simplest way to register services. Input parameters are resolved by type, and the return type becomes the token.

```go
func NewConfig() *Config {
    return &Config{Host: "localhost", Port: 5432}
}

func NewDatabase(cfg *Config) (*Database, error) {
    return Connect(cfg.Host, cfg.Port)
}

func NewUserService(db *Database) *UserService {
    return &UserService{DB: db}
}

c.Register(inject.AutoProvide(NewConfig))
c.Register(inject.AutoProvide(NewDatabase))
c.Register(inject.AutoProvide(NewUserService))
```

Supported constructor signatures:

- `func() T`
- `func() (T, error)`
- `func(dep1 A, dep2 B, ...) T`
- `func(dep1 A, dep2 B, ...) (T, error)`

### Pre-Built Values

Register an existing value by type:

```go
cfg := &Config{Host: "production", Port: 443}
c.Register(inject.ProvideInstance[*Config](cfg))
```

Or with an explicit token:

```go
portToken := inject.Named[int]("port")
c.Register(inject.ProvideValue(portToken, 8080))
```

### Factory-Based (Provide)

For full control, use `Provide` with a factory function and explicit token:

```go
greetToken := inject.Named[string]("greeting")

c.Register(inject.Provide(greetToken, func(r inject.Resolver) (any, error) {
    name, err := inject.ResolveAs[string](r, nameToken)
    if err != nil {
        return nil, err
    }
    return fmt.Sprintf("Hello, %s!", name), nil
}, inject.WithDeps(nameToken)))
```

Use `ResolveAs[T]` inside factories for type-safe dependency resolution.

## Tokens

Tokens identify dependencies in the container. Two tokens are equal if they share the same key.

### Class Tokens (by type)

Used with `AutoProvide`. The Go type is the identity.

```go
token := inject.TokenOf[*UserService]()
```

### Named Tokens

Allow multiple providers of the same type, distinguished by name.

```go
primaryDB := inject.Named[*sql.DB]("primary")
replicaDB := inject.Named[*sql.DB]("replica")
```

Register named providers with `AutoProvideNamed`:

```go
c.Register(inject.AutoProvideNamed("primary", NewPrimaryDB))
c.Register(inject.AutoProvideNamed("replica", NewReplicaDB))

// Resolve by name
val, err := c.Get(inject.Named[*sql.DB]("primary"))
```

Named providers are not resolvable via class tokens. A class token `TokenOf[*sql.DB]()` will not find a named `*sql.DB` provider.

### Tagged Tokens

Tags enable multi-resolution, collecting all providers that share a tag.

```go
c.Register(inject.AutoProvide(NewAuthMiddleware, inject.WithTags("middleware")))
c.Register(inject.AutoProvide(NewLoggingMiddleware, inject.WithTags("middleware")))

// Resolve all middleware
results, err := c.List(inject.Tagged[Middleware]("middleware"))
// results contains both middleware instances
```

## Provider Options

Options configure provider behavior:

| Option | Description |
|--------|-------------|
| `WithScope(scope)` | Set lifetime: `Singleton` (default) or `Scoped` |
| `WithVisibility(vis)` | Set visibility: `Public` (default) or `Private` |
| `WithTags(tags...)` | Add tags for multi-resolution via `List()` |
| `WithDeps(tokens...)` | Declare dependencies for validation and cycle detection |
| `WithOnClose(hook)` | Register a cleanup function called on `Close()` |
| `WithLazy()` | Skip eager resolution at startup |

```go
c.Register(inject.AutoProvide(NewDatabase,
    inject.WithTags("infrastructure"),
    inject.WithOnClose(func() error { return db.Close() }),
))
```

## Scopes and Lifetimes

### Singleton (default)

Resolved once, cached for the container's lifetime. The factory runs exactly once.

```go
c.Register(inject.AutoProvide(NewDatabase)) // singleton by default
```

### Scoped

A new instance is created per scope. Typically used for per-request services (e.g., repositories, transaction contexts).

```go
c.Register(inject.AutoProvideScoped(func(db *Database) *UserRepository {
    return &UserRepository{DB: db}
}))
```

Create a scope with the callback pattern:

```go
err := cc.Scope(ctx, func(ctx context.Context, scope inject.ScopeContext) error {
    repo, err := scope.Get(inject.TokenOf[*UserRepository]())
    if err != nil {
        return err
    }
    // Use repo... scope is automatically closed when this function returns
    return nil
})
```

Or with detached scopes for manual lifetime management:

```go
scope, err := cc.CreateScope()
if err != nil {
    return err
}
defer scope.Close()

val, err := scope.Get(token)
```

### Context-Based Resolution

Scoped containers are attached to `context.Context`. Use `Resolve` to access scoped dependencies from handlers:

```go
func handleRequest(ctx context.Context) {
    svc, err := inject.Resolve[*UserService](ctx, userServiceToken)
    if err != nil {
        // handle error
    }
    // use svc
}
```

### Scoped access from a singleton

A singleton must never depend directly on a scoped provider. Pass the request
context explicitly instead, and resolve the scoped value at the point of use:

```go
reqCtx, err := inject.Resolve[*RequestContext](ctx, requestToken)
```

Validation detects scope violations (singleton depending on scoped) and reports them as errors.

## Container Hierarchy

Containers form a parent-child tree. Child containers inherit public providers from parents.

```go
root := inject.NewContainer("root", nil)
root.Register(inject.ProvideValue(dbToken, sharedDB))

child := root.CreateChild("module-a")
child.Register(inject.ProvideValue(localToken, localService))

// child can resolve dbToken (from parent) and localToken (local)
// root cannot resolve localToken
```

### Visibility

- **Public** (default): visible to the declaring container and all descendants.
- **Private**: visible only within the declaring container. Children cannot access private providers.

```go
root.Register(inject.Provide(secretToken,
    func(_ inject.Resolver) (any, error) { return internalService, nil },
    inject.WithVisibility(inject.Private),
))

// root.Get(secretToken)  -> works
// child.Get(secretToken) -> error: not registered
```

## Validation

Call `Validate` to check the container graph before startup. It detects:

- **Missing dependencies**: a provider declares a dependency that no provider satisfies.
- **Circular dependencies**: A depends on B, B depends on A.
- **Scope violations**: a singleton depends on a scoped provider.

```go
issues := container.Validate()
for _, issue := range issues {
    fmt.Println(issue)
}
```

`ContainerContext.Start()` runs validation automatically before resolving singletons.

### Requirements

Declare tokens that must be present before the container starts:

```go
cc := inject.NewContainerContext("app")
cc.Require(inject.Named[string]("database-url"))

// Start() fails with CodeRequirementNotMet if the token is not registered
err := cc.Start()
```

## Lifecycle

### Startup

`ContainerContext.Start()` performs these steps in order:

1. Check all declared requirements are met.
2. Validate all containers (root + mounted modules) for missing deps, cycles, and scope violations.
3. Eagerly resolve all non-lazy singleton providers.

**Start is transactional on failure.** If any step fails — an unmet
requirement, a validation issue, or a singleton factory returning an error —
Start rolls the partial boot back: every singleton already constructed during
that attempt has its `WithOnClose` hook run (so DB pools, listeners, and other
resources are released instead of leaked), the instance cache is cleared, and
the context returns to the not-started state. A subsequent `Get`/`List`/`Scope`
reports `inject.illegal_state` ("not started") rather than handing out a
half-built graph, and `Start` can be retried once the configuration is fixed.

`StartLazy()` runs steps 1–2 only; it never eagerly resolves singletons, so no
factory runs at start time. Providers are built on first `Get`. The build-time
describe phase uses this so running an app binary with `PUTNAMI_DESCRIBE` set
harvests plugin metadata without reaching external systems.

### Shutdown

`Close()` disposes resources in reverse registration order. Children are closed before parents.

```go
c.Register(inject.AutoProvide(NewDatabase,
    inject.WithOnClose(func() error {
        return db.Close() // called during container.Close()
    }),
))
```

Close hooks run even if some hooks fail. All errors are collected and returned as a single validation error.

## Testing with Forks

`Fork` creates an isolated copy of a `ContainerContext` where you can override specific providers:

```go
func TestUserService(t *testing.T) {
    // cc is the real container context
    forked, err := cc.Fork().
        OverrideValue(inject.Named[string]("database-url"), "sqlite://test.db").
        Override(inject.TokenOf[*Database](), func(_ inject.Resolver) (any, error) {
            return NewInMemoryDatabase(), nil
        }).
        Start()
    if err != nil {
        t.Fatal(err)
    }
    defer forked.Close()

    svc, err := forked.Get(inject.TokenOf[*UserService]())
    // test with mocked dependencies...
}
```

The original container remains unmodified. Forks copy all registrations and apply overrides before starting.

## Field Wiring

`Wire` populates a struct's exported, currently-nil pointer and interface fields
from the container, eliminating manual resolve boilerplate (the framework's
plugin auto-wire phase uses it):

```go
type MyPlugin struct {
    DB  *sql.Pool
    Log *logger.Logger
    raw int         // value type — never wired
    Out io.Writer   `inject:"-"` // excluded from wiring
}

if err := inject.Wire(cc, plugin); err != nil {
    return err
}
// plugin.DB and plugin.Log are now set if their types are registered.
```

Wiring rules:

- Only exported, settable **pointer** and **interface** fields are candidates.
- Already-set (non-nil) fields are never overwritten.
- Fields tagged `inject:"-"` are excluded.
- Value-type and unexported fields are ignored.

`Wire` is **lenient**: a field whose type is not registered is silently skipped
and `Wire` always returns a nil error. This keeps best-effort wiring (optional
dependencies) cheap.

When a missing field is a misconfiguration rather than an optional dependency,
use `WireStrict`. It populates the same fields but returns an error
(`inject.wire_incomplete`) enumerating every wireable field it could not
satisfy, so the problem surfaces at wire time instead of as a later
nil-pointer panic:

```go
if err := inject.WireStrict(cc, plugin); err != nil {
    // err names the unresolved fields, e.g.
    // "could not wire 1 field(s) on MyPlugin: Log"
    return err
}
```

A `nil` container or a non-struct target is a no-op for both functions (nil
error).

## Error Codes

All errors use structured codes from `go.putnami.dev/errors`:

| Code | Meaning |
|------|---------|
| `inject.not_registered` | No provider found for the requested token |
| `inject.circular_dependency` | Circular dependency detected in the provider graph |
| `inject.scope_violation` | Singleton depends on a scoped provider |
| `inject.container_closed` | Operation attempted on a closed container |
| `inject.duplicate_provider` | Same token registered twice in the same container |
| `inject.requirement_not_met` | A required token was not provided |
| `inject.type_mismatch` | Resolved value does not match the expected type |
| `inject.factory_failed` | Factory function returned an error |
| `inject.validation` | Aggregated validation errors |
| `inject.wire_incomplete` | `WireStrict` could not satisfy one or more struct fields |
| `inject.illegal_state` | Operation attempted in the wrong lifecycle state (e.g. `Get` before `Start`, or `Register`/`Start` after start) |

Check errors with `errors.Is`:

```go
_, err := container.Get(token)
if errors.Is(err, inject.CodeNotRegistered) {
    // handle missing provider
}
```

## Best Practices

- **Prefer `AutoProvide`** for most services. Use explicit tokens (`Provide`, `Named`) only when you need multiple providers of the same type or non-struct types.
- **Declare dependencies with `WithDeps`** when using `Provide` so that validation and cycle detection work correctly. `AutoProvide` handles this automatically.
- **Use `WithOnClose`** for any provider that holds resources (database connections, file handles, network listeners).
- **Mark scoped providers as `WithLazy()`** to avoid eager resolution at startup. Scoped providers are meant to be resolved within a scope, not at container creation time.
- **Use `Validate` or `Start`** to catch configuration errors early rather than at runtime resolution.
- **Use `Fork` for testing** instead of building a separate container from scratch. This ensures your test uses the same wiring as production, with only the necessary overrides.
- **Avoid scope violations**: never let a singleton depend directly on a scoped provider. Pass the request context explicitly and resolve the scoped value with `inject.Resolve[T](ctx, token)` instead.

## Contract and compatibility

See the [dependency injection specification](../specs/dependency-injection.json),
[lifetime ADR](adr/0001-lifetimes-own-construction-and-cleanup.md), and [support
evidence](../README.md#support-and-contract). The package is stable and
maintained, with the workspace's [pre-1.0 migration
policy](../../../../RELEASE.md), not strict compatibility between every `0.x`
minor.
