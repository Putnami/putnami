# go.putnami.dev/inject

Hierarchical dependency injection container with constructor-based auto-wiring and token-based registration.

## Quick Start

```go
import "go.putnami.dev/inject"

cc := inject.NewContainerContext("app")
cc.Register(inject.AutoProvide(NewDB))
cc.Register(inject.AutoProvide(NewUserService))

// Register Close before the error check: a failed Start already disposes the
// singletons it managed to build and resets the context to a retryable state,
// so Close is a safe no-op on that path — and any singleton built by a later
// successful retry is still released.
defer cc.Close()
if err := cc.Start(); err != nil {
    log.Fatal(err)
}
```

`Start` is **transactional on failure**: if a non-lazy singleton factory (or
validation, or a requirement check) fails, every singleton already constructed
during that boot has its `WithOnClose` hook run, the instance cache is cleared,
and the context returns to the idle/not-started state. `Get`/`List`/`Scope`
then report "not started" rather than handing out a half-built graph, and
`Start` can be retried after the configuration is fixed.

## Constructor-Based (AutoProvide)

Parameters resolved by type, return type becomes the token:

```go
func NewDB() *DB                          { return &DB{} }
func NewUserService(db *DB) *UserService  { return &UserService{DB: db} }
func NewAPI(db *DB) (*API, error)         { return &API{}, nil } // error variant

cc.Register(inject.AutoProvide(NewDB))
cc.Register(inject.AutoProvide(NewUserService))
cc.Register(inject.AutoProvide(NewAPI))
```

**Constructor-based (`AutoProvide`) is the recommended default.** Token-based is for
named values or interface bindings where type alone is ambiguous.

## Token-Based (Explicit)

```go
portToken := inject.Named[int]("port")
cc.Register(inject.ProvideValue(portToken, 8080))

// Resolve
port, err := inject.Resolve[int](ctx, portToken)
```

## Scoped Resolution

Create child scopes for per-request isolation:

```go
scope, err := cc.CreateScope()
if err != nil {
    log.Fatal(err)
}
defer scope.Close()

ctx := scope.Context(context.Background())
svc, _ := inject.Resolve[*MyService](ctx, myToken)
```

Or run within a managed scope that closes automatically:

```go
err := cc.Scope(ctx, func(ctx context.Context, scope inject.ScopeContext) error {
    svc, err := inject.Resolve[*MyService](ctx, myToken)
    if err != nil {
        return err
    }
    _ = svc
    return nil
})
```

## Testing

Fork a container for test isolation, overriding providers before starting:

```go
test, err := cc.Fork().
    OverrideValue(dbToken, mockDB).
    Start()
if err != nil {
    log.Fatal(err)
}
defer test.Close()
```

## When to Use DI

DI is recommended for any Go server with **3+ services** or cross-cutting concerns
(auth, telemetry, caching). It pays for itself through:

- **Testability**: `Fork` overrides specific providers without rebuilding the graph.
- **Lifecycle management**: `WithOnClose` ensures database pools and storage backends
  are released in the correct order.
- **Explicit dependency graph**: `Start()` validates the graph at boot, catching
  missing or circular dependencies before they surface at runtime.

For libraries or CLI tools with simple dependency graphs, manual wiring is fine.

## Server Wiring Pattern

The recommended pattern for Go servers:

```text
Config → Pool → Repositories → Services → Handlers
         ↘ Storage Backend ↗
```

Each layer is a provider. `AutoProvide` handles most cases; use `Provide` for
config-driven branching (e.g., Postgres vs in-memory) or when a constructor takes
a plain value (like a `string`) that would be ambiguous as a class token.

```go
// main.go
cc := inject.NewContainerContext("my-server")
RegisterProviders(cc, coreCfg, log)

if err := cc.Start(); err != nil {
    log.Fatal(err)
}
defer cc.Close()

svc, _ := cc.Get(inject.TokenOf[*MyService]())
```

## DI and Package Organization

DI works best with **functional packages** — each package owns its domain (types,
repository, service) and exposes interfaces for cross-package dependencies:

```go
// blob/service.go — package owns its full domain
type Repository interface { Store(ctx, b Blob) error }
type Service struct { repo Repository; backend storage.Backend }
func NewService(repo Repository, backend storage.Backend) *Service { ... }

// blob/pg_repo.go — implementation detail, unexported constructor
type pgRepo struct { pool *database.Pool }
func newPgRepo(pool *database.Pool) *pgRepo { ... }
```

Providers register the package's exports — consumers depend on interfaces, not implementations:

```go
cc.Register(inject.AutoProvide(blob.NewService))     // resolves Repository + Backend
cc.Register(inject.Provide(                           // config-driven implementation
    inject.TokenOf[blob.Repository](),
    func(r inject.Resolver) (any, error) {
        pool, _ := inject.ResolveAs[*database.Pool](r, inject.TokenOf[*database.Pool]())
        return blob.NewPgRepo(pool), nil
    },
))
```

This keeps packages decoupled — `blob` never imports `tenant`, the container wires
them. Refactoring a package's internals doesn't affect other packages as long as
the exported interface is stable.

See `doc/getting-started.md` for full reference.

## Maintained contract

This public package is `stable` in the workspace [support
catalog](../../../putnami.support.json). Its behavior is defined by the
[dependency injection specification](specs/dependency-injection.json) and
[lifetime ADR](doc/adr/0001-lifetimes-own-construction-and-cleanup.md). Before
v1.0, follow the workspace [migration-based compatibility
policy](../../../RELEASE.md); do not infer strict compatibility between every
`0.x` minor.
