// Package app provides the application lifecycle, module system, and plugin
// architecture for the Putnami Go framework. Applications are composed of
// modules, which contain plugins that participate in the lifecycle phases:
// auto-wire → configure → start → stop.
package app

import (
	"context"

	"go.putnami.dev/config"
	"go.putnami.dev/inject"
	"go.putnami.dev/migration"
	protocaps "go.putnami.dev/protocol/capabilities"
	protofeatures "go.putnami.dev/protocol/features"
)

// Plugin participates in the application lifecycle. Each method is optional —
// implement only the phases your plugin needs.
//
// Lifecycle phases:
//   - Auto-wire: exported pointer/interface fields resolved from DI (automatic)
//   - Configure: register routes, prepare resources, finalize DI bindings (sequential)
//   - Start: start servers, subscribe to events (parallel)
//   - Stop: graceful shutdown (reverse order)
//
// Exported pointer and interface fields on plugin structs are automatically
// resolved from the DI container before Configure is called. Use the struct
// tag `inject:"-"` to exclude a field from auto-wiring.
type Plugin interface {
	// Name returns a human-readable identifier for this plugin.
	Name() string
}

// Configurer plugins initialize during the configure phase (after DI is built).
// Use this to register routes, finalize handler bindings, and access DI-resolved services.
//
// ctx is the boot context — it is canceled if startup is aborted. Use it for
// boot-time work that may block (KMS client construction, OTLP exporter setup,
// gRPC channel init) instead of reaching for context.Background().
type Configurer interface {
	Plugin
	Configure(ctx context.Context, owner *Module) error
}

// Provider plugins declare DI registrations that are collected before the
// container is built. Use this to let plugins self-register their services
// without requiring separate app.ProvideFunc() calls.
type Provider interface {
	Plugin
	Provides() []inject.Registration
}

// MigrationContributor is implemented by plugins that own migrations of any
// kind. Returning multiple Sources is the supported stacking pattern: a
// feature can mix embed.FS-backed paired SQL files with inline definitions,
// and contribute to several kinds (SQL today; GCS / document / cache /
// event-topic kinds later) in the same plugin.
//
// The application's lifecycle walks every MigrationContributor between
// the Configure and Migrate phases, AddSources their return into the
// per-app *migration.Registry, and the runners registered for each kind
// pick up the work from there.
type MigrationContributor interface {
	Plugin
	MigrationSources() []migration.Source
}

// ConfigContributor is implemented by plugins that own one or more config
// blocks. The describe phase walks every ConfigContributor in the module tree
// and aggregates their blocks into the workload's published config schema, so a
// library can own its config — and the secrets its sensitive fields declare —
// and any workload that composes it publishes them transitively. This is the
// config dual of MigrationContributor: the same "compose the plugin, get its
// contribution in the build artifact" wiring, with no per-workload restatement.
//
// Return the type-erased descriptor of each config.Definition the plugin owns:
//
//	var coreSchema = config.Config[CoreOptions]("core")
//
//	func (p *Plugin) ConfigDefinitions() []config.Descriptor {
//	    return []config.Descriptor{coreSchema.Descriptor()}
//	}
//
// The block's fields (including default/env/sensitive/validate metadata) are
// reflected from the schema type, matching what the source extractor derives
// from the same declaration in the owning module.
type ConfigContributor interface {
	Plugin
	ConfigDefinitions() []config.Descriptor
}

// CapabilityRequirement declares one logical capability and the contribution
// kinds that must be present in the build-time capability manifest. The
// emitter supplies provenance from the contributing plugin; callers only name
// the logical capability and its required provider kinds.
type CapabilityRequirement struct {
	Name     string
	Requires []protocaps.CapabilityKind
}

// RequiredCapabilityContributor is implemented by plugins that need the build
// to prove a capability is complete. For example, a SQL-backed module can
// require datasource, migration, and readiness providers. Describe fails
// closed when any required provider is absent.
type RequiredCapabilityContributor interface {
	Plugin
	RequiredCapabilities() []CapabilityRequirement
}

// CapabilitySchema is static schema inventory owned by a plugin. The app
// emitter stamps provenance from the resolved project-package graph; providers
// declare only their stable name, schema kind, and route/artifact path.
type CapabilitySchema struct {
	Name string
	Kind protocaps.SchemaKind
	Path string
}

// CapabilityDiscoverer is static discovery inventory owned by a plugin.
type CapabilityDiscoverer struct {
	Name string
	Kind protocaps.DiscovererKind
}

// CapabilityInventoryContributor lets framework adapters expose contribution
// kinds that cannot be inferred from lifecycle interfaces alone. It is app-
// owned to avoid dependency cycles: api/openapi/proto and dependency plugins
// implement it while the app describer remains framework-agnostic.
type CapabilityInventoryContributor interface {
	Plugin
	CapabilitySchemas() []CapabilitySchema
	CapabilityDiscoverers() []CapabilityDiscoverer
}

// ContributionRef names one contribution of the project being built. It
// deliberately carries no owner project: the describe producer owns exactly one
// project and stamps it, so a workload cannot claim a contribution belonging to
// one of its dependencies.
type ContributionRef struct {
	// Kind is the contribution kind, from the capability v2 vocabulary.
	Kind protocaps.ContributionKind
	// Subkind is the contribution subkind, empty for the kinds that forbid one.
	Subkind string
	// Key is the contribution key.
	Key string
}

// FeatureProof associates one authored maturity requirement of the declaring
// feature with one exact contribution that supports it.
//
// This is the whole authoring surface for generated technical evidence. The
// stage comes from the authored requirement in putnami.features.json, and the
// evidence ID, issuer, source binding, provenance, and subject are computed by
// the build — so a declaration can state which fact proves which requirement,
// and nothing else.
type FeatureProof struct {
	// Requirement is the feature-local requirement ID being supported.
	Requirement string
	// Contribution is the exact fact that supports it.
	Contribution ContributionRef
}

// DomainAccessContract is one Domain Access & Replication Contract a plugin
// enforces at run time.
//
// It is EVIDENCE, not authority. The contract itself is declared and reviewed in
// a `putnami.architecture.json`; a row here only states that a running component
// was configured with it, so `putnami architecture validate` can tell a declared
// import nothing implements from an implemented one nobody declared. Emitting a
// row never creates a permission — that is the anti-pattern ADR 0001 of
// protocols/architecture exists to forbid.
//
// Every member is carried VERBATIM from the declared contract. This package does
// not restate the ARC/DARC vocabulary and never validates it: what a mode or a
// failure behavior means belongs to `go.putnami.dev/protocol/architecture`, and a
// second copy is exactly the drift a checker joining the two documents exists to
// catch. `go.putnami.dev/app/darc` builds these values from a contract it already
// validated through that protocol.
type DomainAccessContract struct {
	// Import is the architecture import ID the component enforces.
	Import string
	// Mode is the declared access mode.
	Mode string
	// Status is the declared lifecycle status of the import.
	Status string
	// Transports lists the carriers the contract declares, each with its role.
	Transports []DomainAccessTransport
	// Enforced records the contract parameters the component actually applies.
	Enforced DomainAccessEnforcement
}

// DomainAccessTransport is one declared carrier of an enforced contract. Role is
// the carrier's part in the contract — a projection declares bootstrap and
// updates, the other modes declare one transport.
type DomainAccessTransport struct {
	Role         string
	Kind         string
	Contract     string
	Availability string
}

// DomainAccessEnforcement records the contract parameters a component applies.
// Every member is optional: a reference declares none of them, a projection
// declares them all.
type DomainAccessEnforcement struct {
	MaxStaleness string
	OnMissing    string
	OnStale      string
	Ordering     string
	LateEvents   string
	Deletion     string
	Writer       string
	Rebuild      string
}

// DomainAccessContributor is implemented by plugins that carry runtime-enforced
// domain access contracts into the describe pass. It is app-owned for the same
// reason InfraContributor is: the descriptor must be declarable without app and
// the contributing package depending on each other.
//
// Implementations must return static metadata, be deterministic, and perform no
// I/O. Nothing they return participates in configure, start, stop, dependency
// injection, or request handling.
type DomainAccessContributor interface {
	Plugin
	DomainAccessContracts() []DomainAccessContract
}

// DesignContributor exposes native, build-time-only technical facts owned by a
// framework plugin. The app supplies a builder already scoped to the plugin's
// owning module and feature, so contributors never repeat feature metadata.
// Implementations must be deterministic and perform no I/O.
type DesignContributor interface {
	Plugin
	ContributeDesign(builder *DesignBuilder) error
}

// DesignInfraRequirement is one bounded infrastructure fact already declared
// by a native framework registration. Name is the logical resource identity;
// Kind reuses the Capability Manifest's closed infrastructure vocabulary.
// Provenance is optional because not every registry retains its call site.
// Values are observed only during describe and never affect runtime activation.
type DesignInfraRequirement struct {
	Name       string
	Kind       protocaps.InfraKind
	Provenance *protofeatures.DesignProvenance
}

// InfraContributor lets a plugin expose the infrastructure requirements its
// ordinary native registrations already own without granting it arbitrary
// graph mutation. Use DesignContributor only for a fact this bounded seam
// cannot represent.
type InfraContributor interface {
	Plugin
	DesignInfraRequirements() []DesignInfraRequirement
}

// DesignTestKind is the bounded classification of an explicitly registered
// test. Test files are never inferred from paths or naming conventions.
type DesignTestKind string

const (
	// DesignTestUnit is one isolated component test.
	DesignTestUnit DesignTestKind = "unit"
	// DesignTestIntegration exercises collaboration between components.
	DesignTestIntegration DesignTestKind = "integration"
	// DesignTestConformance exercises a shared behavioral contract.
	DesignTestConformance DesignTestKind = "conformance"
	// DesignTestE2E exercises a complete user-visible flow.
	DesignTestE2E DesignTestKind = "e2e"
)

// DesignTestProof binds one declared test to an authored executable-spec
// check: the feature, the feature-local requirement, and the exact check ID
// from the manifest's verification criterion. It is discoverability
// only — the graph shows which declared test is EXPECTED to protect which
// check, so `sdd.feature_context` can answer "which test proves this?", but
// a declared binding can never itself prove a passing test: only the test
// adapter's observed verdict does.
type DesignTestProof struct {
	Feature     string
	Requirement string
	Check       string
}

// DesignTest is one stable, declared test identity. Name must be semantic and
// unique within the project graph; Provenance identifies the declaration when
// the native test registry retains it; Proves optionally names the authored
// executable-spec checks the test protects.
type DesignTest struct {
	Name       string
	Kind       DesignTestKind
	Proves     []DesignTestProof
	Provenance *protofeatures.DesignProvenance
}

// TestContributor is the narrow native registration seam for declared tests.
// It deliberately cannot infer or scan test files and cannot mutate the graph.
type TestContributor interface {
	Plugin
	DesignTests() []DesignTest
}

// TypedClientContributor is implemented by hand-written clients that know which
// producer they call. It is app-owned for the same reason CapabilitySchema is:
// the descriptor must be declarable by any client package without that package
// and app depending on each other.
//
// The describe pass derives one `calls` relationship per module that composes
// the client — through Use or app.Contribute — so consumer attribution comes
// from actual composition rather than from parsing URLs, package names, or
// imports. A hand-written client is published as a typed-client node and is
// never presented with generated-client authority.
//
// Implementations must return static metadata, be deterministic, and perform no
// I/O. TypedClientDesign is read only during describe; nothing it returns
// participates in configure, start, stop, dependency injection, or request
// handling. See typed_client_design.go for the descriptor.
type TypedClientContributor interface {
	Plugin
	TypedClientDesign() TypedClientDesign
}

// HealthChecker is implemented by plugins that probe the liveness of a
// dependency they own — a connection pool, a cache, an upstream service.
// Implementations are discovered by liveness endpoints (e.g. /healthz on
// the platform plugin) by walking the module tree; consumers should not
// have to hand-register a probe for every plugin that ships one.
//
// CheckHealth answers "is this dependency in a state I can't recover from
// without a restart?" — returning an error here causes orchestrators
// (Kubernetes liveness probe) to restart the pod. Use it sparingly: for
// readiness-style conditions ("this dependency is temporarily down, drain
// traffic but don't restart"), implement ReadinessChecker instead.
//
// CheckHealth must be safe to call concurrently and must respect ctx
// cancellation. Return nil when the dependency is healthy; return a
// descriptive error otherwise. CheckHealth runs on the request path of
// liveness endpoints, so probes must be lightweight (sub-second timeouts,
// cheap round-trips).
type HealthChecker interface {
	Plugin
	CheckHealth(ctx context.Context) error
}

// ReadinessChecker is implemented by plugins that report whether they
// are ready to serve traffic — caches warmed, downstream connections
// established, leader election complete, etc. Implementations are
// discovered by readiness endpoints (e.g. /readyz on the platform
// plugin) by walking the module tree.
//
// CheckReadiness answers "can this dependency serve traffic right now?"
// — returning an error here causes orchestrators (Kubernetes readiness
// probe) to drain traffic from the pod without restarting it. This is
// the right interface for transient warmup states or dependency
// flapping that doesn't need a process restart to recover.
//
// CheckReadiness must be safe to call concurrently and must respect ctx
// cancellation. Return nil when ready; return a descriptive error
// otherwise. Probes run on the request path of readiness endpoints, so
// must be lightweight (sub-second timeouts, cheap round-trips).
type ReadinessChecker interface {
	Plugin
	CheckReadiness(ctx context.Context) error
}

// Starter plugins run during the start phase (after DI is built).
//
// ctx is the application context — it lives for the application's lifetime and
// is canceled when shutdown begins.
type Starter interface {
	Plugin
	Start(ctx context.Context, owner *Module) error
}

// Stopper plugins clean up during shutdown.
//
// ctx is the shutdown context — it carries the shutdown deadline so Stop can
// honor a bounded graceful-drain window.
type Stopper interface {
	Plugin
	Stop(ctx context.Context, owner *Module) error
}

// Describer plugins emit build-time artifacts (proto schemas, openapi specs,
// gRPC stubs, etc.) when the application runs in describe mode.
//
// Describe is invoked after Configure but before Start when PUTNAMI_DESCRIBE
// is set in the environment, and the application exits before any servers or
// background workers run. Plugins should produce deterministic output so the
// generated artifacts are safe to commit.
//
// Implementations must NOT have side effects beyond writing under
// DescribeContext.OutputDir — connecting to databases, opening sockets, or
// touching the filesystem outside OutputDir is a contract violation.
type Describer interface {
	Plugin
	Describe(ctx *DescribeContext) error
}

// AdditionalDescribeTargeter lets a describer prepare in-memory state needed
// by another build target without emitting its own artifact. The application
// still passes the original context, so implementations can distinguish their
// primary target from a dependency target and keep writes scoped accordingly.
type AdditionalDescribeTargeter interface {
	AdditionalDescribeTargets() []string
}

// DescribeContext is passed to each Describer when running in describe mode.
type DescribeContext struct {
	// OutputDir is the absolute directory where artifacts must be written.
	// Plugins write to <OutputDir>/<rel-path> using their own subdirectory
	// convention (e.g. "schema/api.proto"). The runner is responsible for
	// creating the directory; plugins create their own subdirectories.
	OutputDir string

	// Targets is the comma-split value of PUTNAMI_DESCRIBE. An empty list
	// (or a single "all" entry) means "run all describers". Otherwise only
	// describers whose name appears in the list run.
	Targets []string
}

// Wants reports whether the named describer should run given ctx.Targets.
// Plugins typically call Wants(p.Name()) at the top of their Describe and
// return nil early when they're not the requested target.
func (c *DescribeContext) Wants(name string) bool {
	if len(c.Targets) == 0 {
		return true
	}
	for _, t := range c.Targets {
		if t == "all" || t == name {
			return true
		}
	}
	return false
}
