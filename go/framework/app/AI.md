# go.putnami.dev/app

Application lifecycle, plugin system, module composition, and DI wiring.

## Quick Start

```go
import "go.putnami.dev/app"

a := app.New("my-service")
a.Use(myPlugin)         // register plugins
a.Use(myModule)         // compose modules
a.ListenAndServe()      // blocks until SIGINT/SIGTERM
```

## Lifecycle Phases

Phase-major: each phase completes across the whole module tree before the next.
Module hooks (`OnPreConfigure` / `OnPostConfigure` / `OnStart` / `OnStop`) bracket
the plugin phases. All take `context.Context`.

| Phase | Interface / Hook | Order | Purpose |
| ----- | ---------------- | ----- | ------- |
| **Modules.PreConfigure** | `OnPreConfigure` | top-down | Pre-DI setup; `Module.Container()` is nil here |
| **DI Build** | (automatic) | — | Build DI container, set `Module.Container()`, auto-wire plugin fields |
| **Plugins.Configure** | `Configurer` | sequential | Register routes, finalize DI bindings (DI available) |
| **Modules.PostConfigure** | `OnPostConfigure` | bottom-up | Coordinate a module's plugins once all are wired (DI available) |
| **Describe** | `Describer` | — | Build-time only — emit artifacts (proto, openapi, …) and exit before Start |
| **Migrate** | (automatic) | — | Apply pending migrations across every registered kind |
| **Invoke** | (automatic) | — | Run `InvokeFunc` callbacks |
| **Plugins.Start** | `Starter` | parallel | Start servers, open connections |
| **Modules.OnStart** | `OnStart` | top-down | Startup that depends on a module's plugins running |
| **Modules.OnStop** | `OnStop` | bottom-up | Cleanup before the module's plugins stop |
| **Plugins.Stop** | `Stopper` | reverse | Graceful shutdown |

## Plugins

Implement `Name() string` plus any lifecycle interface:

```go
type MyPlugin struct{}
func (p *MyPlugin) Name() string { return "my-plugin" }
func (p *MyPlugin) Configure(ctx context.Context, owner *app.Module) error { /* register routes, DI available */ return nil }
func (p *MyPlugin) Start(ctx context.Context, owner *app.Module) error     { /* start serving */ return nil }
func (p *MyPlugin) Stop(ctx context.Context, owner *app.Module) error      { /* cleanup */ return nil }
```

### Auto-Provide

Plugins can self-register DI providers via the `Provider` interface:

```go
func (p *MyPlugin) Provides() []inject.Registration {
    return []inject.Registration{inject.AutoProvide(NewMyService)}
}
```

### Capability Interfaces

Features opt into runtime capabilities by implementing app-owned interfaces and
registering those implementations through their owning plugin:

```go
type MigrationContributor interface { MigrationSources() []migration.Source }
type ConfigContributor    interface { ConfigDefinitions() []config.Descriptor }
type HealthChecker        interface { CheckHealth(ctx context.Context) error }
type ReadinessChecker     interface { CheckReadiness(ctx context.Context) error }
type CapabilityInventoryContributor interface {
    CapabilitySchemas() []app.CapabilitySchema
    CapabilityDiscoverers() []app.CapabilityDiscoverer
}
```

Plugins can also make build-time completeness fail closed:

```go
func (p *SQLPlugin) RequiredCapabilities() []app.CapabilityRequirement {
    return []app.CapabilityRequirement{{
        Name: "sql",
        Requires: []capabilities.CapabilityKind{
            capabilities.CapabilityKindDatasource,
            capabilities.CapabilityKindMigration,
            capabilities.CapabilityKindReadiness,
        },
    }}
}
```

Describe rejects missing requirements and duplicate/conflicting providers with
stable `capabilities.*` diagnostic codes, and publishes no manifest on failure.
Every v2 contribution is keyed by `(ownerProject, kind, subkind, key)`; observed
runtime-interface contributions default to the workload project, while copied
dependency contributions retain their original owner. SQL migration providers
are keyed by migration kind and namespace, so several compatible SQL source
fragments can merge under one namespace and datasource without changing
migration IDs. Conflicting SQL schema declarations fail closed; non-SQL kinds
retain one provider per source. Two different migration kinds may also
intentionally reuse a namespace.

The built-in capability describer also merges strict dependency manifests from
the scheduler-stamped transitive project graph, keeps resolved package versions
in `.gen/version.json` rather than the durable capability manifest,
derives config/migration discoverers, and projects every infra kind from
typed `.gen/infra/*.json` sidecars. OpenAPI/proto schema inventory is published
only when the corresponding artifact exists under the describe output. Each
provenance record names an exact declaration and owner-scoped identity;
dependency protocol-v1 manifests are rejected because they cannot supply those
fields without a lossy projection.

The scheduler stamp covers workspace projects only, so a contributor from a
framework module the workload *requires* (rather than checks out as a sibling
project) is resolved from the binary's own module graph instead. Its provenance
addresses the producer by published identity — `package` is the module path and
`declaration.root` is `package` with a module-root-relative path. Capability
manifests embed neither resolved versions nor the scheduler's volatile
`source-v1` digest; indexing systems resolve both from the build snapshot and
source revision they select. Stable version-independent `packages`
contributions remain in the manifest so `package` requirements are satisfiable.
Only released modules qualify, and `replace` is
read by form rather than by presence: a versioned replacement redirects to
another released module and stays published — recorded under the module that
supplied the code, still matched on the import path it is required by, since
`go mod vendor` files it there — while a directory or `go.work` replacement is
local source. A contributor that is neither stamped nor published still fails
closed. Historical dependency manifests may contain `provenance.version`, but
it is ignored and removed when the aggregate is canonicalized. Nothing derived
from a module-cache location enters the manifest, so the
same commit describes identically on any machine and under `-trimpath`,
`-mod=vendor`, or a plain build.

`.gen/version.json` scheduler metadata still carries source bindings for
generated feature evidence; those bindings are not copied into the capability
manifest.

A complete scheduler stamp from before `sourceRoot`/`sourceBinding` existed is
treated as a no-publication compatibility boundary: describe removes stale
generated Go capability/evidence artifacts and succeeds without emitting v2.
This lets a hosted runner plan with the previous CLI while compiling the newer
framework. Partially upgraded, unavailable, or malformed source metadata still
fails closed.

### Native feature authoring

Declare a product outcome once with `Module.Feature(...)`, then compose its API,
event, migration, data, DI, and generated-client registrations normally. The
describe pass derives `.gen/design/graph.json` from those native registrations;
do not repeat their identities, source paths, artifacts, or feature associations
in wrapper plugins. The [task API](../../samples/task-api) demonstrates the
routine path, and the [capabilities proof](../../samples/capabilities-proof)
shows migration/database and Capability Manifest v2 provenance under the same
scope.

A hand-written client that calls another project's API implements
`TypedClientContributor` and returns a `TypedClientDesign` naming the producer
project, the client's stable name, and its declared operations (contract
templates, never deployment URLs). Describe publishes one `client.typed` node —
never a `client.generated` one — and one `calls` edge per module that composes
the client, through `Use` or `app.Contribute`, so consumers are attributed from
real composition instead of parsed URLs, package names, or imports. Leave
`ProducerFeature` empty when it is not known: those call edges are published with
the `currently-unmodeled` authority rather than a guessed lineage. An empty
`ProducerProject` or `Client` fails the build with an actionable message. Never
put a token, header, body, base URL, or environment value in the descriptor;
node properties are limited to producer, feature, language, and the canonical
operation list.

Configuration definitions, infrastructure requirements, lifecycle phases, and
declared tests are projected without `DesignContributor`. Configuration reuses
`ConfigContributor`; resource plugins expose the bounded `InfraContributor`;
explicit test registries may expose `TestContributor`; configure/start/stop are
observed from the existing lifecycle interfaces. Their semantic nodes carry no
registration provenance, so several modules can share one identity; exact
declaration provenance belongs on each module's `contains` edge. Never infer a
test from a filename or use callback order as lifecycle identity.

Capability provenance is derived from the concrete contributing method and
native generated artifacts; there is no parallel metadata writer. When the
framework cannot observe a technical fact through a native registration, implement the
build-only `DesignContributor` escape hatch on the plugin that owns that fact.
Human attestations remain separate protocol evidence and must not substitute for
technical facts the describer can derive.

### Proving a maturity requirement

To let a technical fact earn maturity, declare the association beside the
feature and let describe emit the evidence:

```go
a.Feature(app.Feature{
    ID: "telemetry-putnami-dev/cli-usage-receiver", /* … */
    Proves: []app.FeatureProof{{
        Requirement:  "expiry-runs-without-the-service",
        Contribution: app.ContributionRef{Kind: protocaps.ContributionKindMigration, Subkind: "sql", Key: "cliagg"},
    }},
})
```

The requirement must already exist in the project's `putnami.features.json` and
accept `capability` evidence; describe reads its stage from there. Everything
else on the record — ID, issuer (`build`, `go.putnami.dev/app`), source binding,
provenance, and subject — is computed from the build. `ContributionRef` carries
no owner project, so a workload cannot claim a dependency's contribution.

An unknown feature or requirement, an unpublished or dependency-owned
contribution, or two proofs that disagree fails describe and publishes neither
the manifest nor the evidence. `Proves` is inert: it is read only during
describe and never reaches configure, start, stop, DI, or request handling.

A contribution nobody proves stays `unclassified` in `putnami features validate`,
which is the intended visible state — see
[ADR 0003](../../../protocols/features/doc/adr/0003-generated-feature-evidence.md).
Never hand-write an evidence fragment to clear one.

The graph is disposable and atomically republished; Capability Manifest v2
remains the durable technical identity/provenance contract. Neither feature nor
design metadata is consulted during configure, start, stop, or request handling.

For maintenance and review, use `putnami features validate` and
`putnami features inspect <feature-id>` instead of loading the full evidence
corpus. See the protocol's
[authoring boundary](../../../protocols/features/README.md#authoring-boundary)
for the split between product intent, technical evidence, and human authority.

| Interface           | When                                                                     | Mounted at                                |
| ------------------- | ------------------------------------------------------------------------ | ----------------------------------------- |
| `HealthChecker`     | Dependency is broken in a way only a restart fixes (liveness)            | `/healthz` (platform), `/_/health` (http) |
| `ReadinessChecker`  | Dependency is temporarily down — drain traffic but don't restart         | `/readyz` (platform)                      |

Implementations are auto-discovered: any plugin in the module tree that
satisfies the interface is registered under its `Name()`. Central application
code does not need to hand-register probes for every feature.

When a probe only needs DI-managed dependencies, contribute a function instead
of stashing resolved dependencies on the plugin:

```go
func (p *Plugin) Configure(ctx context.Context, owner *app.Module) error {
    app.Contribute[app.ReadinessChecker](owner, app.HealthFunc("db", func(ctx context.Context, pools *psql.Pools) error {
        pool, err := pools.For("configs")
        if err != nil {
            return err
        }
        return pool.Ping(ctx)
    }))
    return nil
}
```

Use `HealthChecker` sparingly — failing it triggers Kubernetes pod restarts.
Use `ReadinessChecker` for transient warmup / dependency-flapping states; it
drains traffic without restarting.

### Describe (build-time codegen)

Plugins that emit committed artifacts (proto schemas, openapi specs, gRPC stubs, …)
implement `Describer` instead of writing files in `Configure`. The describe phase
runs `Configure` for every plugin (so discovery completes), then invokes
`Describe(ctx)` on every `Describer`, then exits — `Start` is never called and no
servers, listeners, or background workers run.

```go
type Describer interface {
    Plugin
    Describe(ctx *DescribeContext) error
}

type DescribeContext struct {
    OutputDir string   // absolute directory to write under
    Targets   []string // e.g. ["proto"]; empty/"all" means run every describer
}
```

Describe is triggered from the build-time job by setting two environment variables
before invoking the compiled binary:

| Variable | Purpose |
| -------- | ------- |
| `PUTNAMI_DESCRIBE` | Comma- or space-separated list of describer names, or `all` |
| `PUTNAMI_DESCRIBE_OUT` | Absolute output directory (defaults to `<cwd>/.gen`) |

When `PUTNAMI_DESCRIBE` is set, `Application.ListenAndServe` runs `Describe` and
exits with the describer error (or zero on success). Direct callers can also use
`a.Describe(outputDir, targets)` to run the phase programmatically (used by tests).

During build-triggered describe mode, migration bundles use the authored
`PUTNAMI_PROJECT_NAME` as their application identity, so publication selects the
same project even when `app.New` uses a shorter runtime name. Programmatic
describe calls keep the runtime application name.

Plugins must scope all writes under `ctx.OutputDir`; touching anything else (DBs,
sockets, the project tree) is a contract violation that breaks build-time generation.

The describe phase also runs framework built-in describers that aggregate
capabilities across the whole module tree: `MigrationContributor` sources become
the migration artifacts, and `ConfigContributor` blocks are reflected and emitted
as `.gen/config-deps.json`, which the build merges into the workload's published
`schema/config.json` (and its secrets requirements). A library that implements
either interface is published transitively by every workload that composes it —
no per-workload restatement. A dependency-owned config block must use a path the
workload does not already define.

### Describe-only contributions (`UseForDescribe`)

A `Describer` only runs when its plugin is registered with `Use`. That breaks for
plugins registered **conditionally on runtime config** — a common shape for
infrastructure that has a memory backend in dev/test and a SQL backend in prod:

```go
if cfg.UsesPostgres() {
    a.Use(database.NewPlugin(prodConfig)) // only registered when prod config is active
}
```

The describe phase resolves a dev/test config, so the guarded plugin never
registers, never describes, and the committed `infra/requirements.json` is missing
the inferred requirement, and the aggregated `.gen/requirements.json` omits the
workload's real production needs. `UseForDescribe` is the describe-only dual of
`Use`: the plugin's `Describe` runs, but it is **never** auto-wired, configured,
started, stopped, or added to the DI container — it has no runtime cost, opens no
connections, and is invisible to every `Collect[T]` capability walk (health
probes, migration sources, …).

```go
p := database.NewPlugin(prodConfig)
if cfg.UsesPostgres() {
    a.Use(p)            // full lifecycle in production
} else {
    a.UseForDescribe(p) // describe-only in dev/test builds
}
```

A producer emits from its construction-time configuration. State that is only
populated by the runtime lifecycle — e.g. database schemas attributed from
migration sources collected during `Configure` — is not available in describe-only
mode; declare such inputs on the plugin's config instead (e.g. `Pool.Schemas`).

### Accessing DI in Configure (preferred)

Use `owner.Container()` to access the DI container in Configure — no extra interface needed:

```go
func (p *MyPlugin) Configure(ctx context.Context, owner *app.Module) error {
    cc := owner.Container()
    svc, err := cc.Get(inject.TokenOf[*MyService]())
    // ...
}
```

## Modules

Group plugins, DI providers, and sub-modules:

```go
api := app.NewModule("api").Path("/api")
admin := app.NewModule("admin").Path("/admin")
api.Use(admin)

a := app.New("my-service")
a.Use(api)
```

## Package Organization

Organize packages by **functional domain**, not by technical layer. Each package owns
its types, repository, service, and handlers — keeping internals private and exposing
a controlled public surface.

**Do:**

```text
auth/           → authenticator, middleware, principal, tokens
blob/           → Blob type, repository, service, storage integration
tenant/         → Tenant type, repository, schema manager, isolation middleware
publication/    → orchestrator, adapters, state machine
```

Each package is a self-contained domain that controls what it exports.

**Don't:**

```text
persistence/    → ❌ all repositories grouped by technical role
handlers/       → ❌ all HTTP handlers in one place
services/       → ❌ all business logic lumped together
models/         → ❌ all types divorced from their behavior
```

Layered folders scatter a single domain across the codebase, making it harder to
understand, refactor, or replace.

**Guidelines:**

- A package's name should describe **what it does**, not **how it does it**
- Keep types, constructors, repository, service, and handlers together in the same package
- Export only what consumers need — interfaces for cross-package dependencies, concrete types for internal use
- Use DI (`inject.AutoProvide`) to wire packages together without coupling them
- When a package grows, split by sub-domain (e.g., `publication/mirror/`), not by layer

**Example — a domain package with DI:**

```go
// Package blob manages content-addressed binary storage.
package blob

// Public surface: types + service + repository interface
type Blob struct { ... }
type Repository interface { ... }
type Service struct { repo Repository; backend storage.Backend }

func NewService(repo Repository, backend storage.Backend) *Service { ... }

// Private: implementation details
type pgRepo struct { pool *database.Pool }
func newPgRepo(pool *database.Pool) *pgRepo { ... }
```

Register in DI — consumers depend on the interface, not the implementation:

```go
cc.Register(inject.AutoProvide(blob.NewService))
cc.Register(inject.Provide(
    inject.TokenOf[blob.Repository](),
    func(r inject.Resolver) (any, error) { return blob.NewPgRepo(pool), nil },
))
```

## Dependency Injection

### Constructor-based (fx-style)

```go
a.ProvideFunc(NewDB, NewUserService)
a.InvokeFunc(func(users *UserService) {
    fmt.Println("ready:", users)
})
```

### Token-based

```go
token := inject.Named[string]("app-name")
a.Provide(inject.ProvideValue(token, "my-service"))
```

### Scoped (per-request)

```go
a.ProvideScopedFunc(NewRequestLogger)
```

## Default Path (Recommended)

Use **constructor-based DI** (`ProvideFunc` / `AutoProvide`) as the default:

```go
a := app.New("my-service")
a.ProvideFunc(NewDB, NewUserService)
a.Use(server)
a.ListenAndServe()
```

Token-based DI and `InvokeFunc` are available for advanced use cases (named values,
side-effect callbacks), but constructor-based DI should be the first choice.

## Startup Validation

Run DI Build + Configure + Invoke without starting servers — useful for tests and CI:

```go
if err := a.Validate(); err != nil {
    log.Fatal("startup validation failed:", err)
}
```

## Custom Runner

```go
a.Run(func(ctx context.Context) error {
    // runs after all plugins start, ctx canceled on shutdown
    <-ctx.Done()
    return nil
})
```

See `doc/getting-started.md` for full reference.

## Domain access contracts (`app/darc`)

The `darc` sub-package turns a declared Domain Access & Replication Contract into
a component that enforces it at runtime — the consumer half of an ARC/DARC
manifest, as code:

```go
projection, err := darc.NewProjection[WorkspaceContext](workspaceContextImport,
    darc.WithBootstrap(loadFromRuntimeAPI))
record, found, err := projection.Get(ctx, workspaceID)   // freshness + missing/stale applied
writer, err := projection.Writer("observability.workspace-context-projector")
```

Construction runs the protocol's own verdict, so a contract `putnami architecture
validate` would reject never reaches the runtime. `Projection`, `Snapshot`, and
`Command` are ordinary values, wired with the usual DI conventions. The
components themselves live at `go.putnami.dev/protocol/architecture/darc`
(framework-free, so any Go program can enforce a contract); this sub-package
re-exports them unchanged and adds the describe carrier below.

A registered component contributes one machine row to the project's capability
manifest through `a.Use(darc.NewPlugin("contracts", projection, reference))`, so
`putnami architecture validate` can tell a declared contract nothing implements
from an implemented one nobody declared. The row is evidence, never authority.

It moves no data — the consumer supplies bootstrap and updates over whichever
carrier the contract declares — and it decides nothing about permissions.

`go.putnami.dev/protocol/architecture` is **experimental**, and this sub-package
moves with it. See
[Domain access contracts](../../doc/framework/24-domain-access-contracts.md).

## Maintained contract

This public package is `stable` in the workspace [support
catalog](../../../putnami.support.json). Its behavior is defined by the
[application lifecycle specification](specs/application-lifecycle.json) and
[phase-major lifecycle ADR](doc/adr/0001-phase-major-lifecycle.md). Before v1.0,
follow the workspace [migration-based compatibility policy](../../../RELEASE.md);
do not infer strict compatibility between every `0.x` minor.
