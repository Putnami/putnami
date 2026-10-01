package app

import (
	"context"
	"fmt"

	"go.putnami.dev/inject"
)

// Module groups plugins, DI providers, and sub-modules into a composable unit.
// Modules form a tree: each module may contain child modules and plugins.
//
// Beyond holding plugins, a Module has a lifecycle of its own that the App
// orchestrator drives, phase-major, around the plugin phases:
//
//  1. Modules.PreConfigure   (top-down: root → leaves)   — before DI build
//  2. DI graph build
//  3. Plugins.Configure
//  4. Modules.PostConfigure  (bottom-up: leaves → root)  — DI container ready
//  5. Migrate
//  6. Invoke
//  7. Plugins.Start
//  8. Modules.OnStart        (top-down, after their plugins)
//     ─── running ───
//  9. Modules.OnStop         (bottom-up reverse)
//  10. Plugins.Stop
//  11. DI close
//
// Register hooks fluently with OnPreConfigure / OnPostConfigure / OnStart /
// OnStop. All hooks are optional; a module with none behaves as a passive
// container, exactly as before.
type Module struct {
	name               string
	path               string
	feature            *Feature
	plugins            []Plugin
	describeOnly       []Describer
	modules            []*Module
	registrations      []inject.Registration
	contributions      []contribution
	preConfigureHooks  []func(context.Context) error
	postConfigureHooks []func(context.Context, *inject.ContainerContext) error
	startHooks         []func(context.Context) error
	shutdownHooks      []func(context.Context) error
	parent             *Module
	cc                 *inject.ContainerContext
}

// Feature is the only product-level declaration needed to attach a stable
// outcome to a native module. API, event, data, service, and generated-client
// surfaces are derived from the module's ordinary framework composition.
// Feature metadata never changes runtime activation or request handling.
type Feature struct {
	ID      string
	Name    string
	Outcome string
	Owner   string

	// Proves associates authored maturity requirements of this feature with the
	// exact contributions that support them, so a build producer can emit
	// Feature Evidence instead of leaving the fact unclassified. It is inert
	// observational metadata: it is read only during describe, and adding,
	// removing, or reordering it cannot change configuration, startup,
	// dependency injection, routing, migrations, or shutdown.
	Proves []FeatureProof

	provenance designSource
}

// NewModule creates a new named module.
//
//	m := app.NewModule("users")
func NewModule(name string) *Module {
	return &Module{name: name}
}

// Name returns the module name.
func (m *Module) Name() string {
	return m.name
}

// Path sets the base path prefix for all routes in this module.
//
//	m.Path("/api/v1")
func (m *Module) Path(path string) *Module {
	m.path = path
	return m
}

// pathPrefix returns this module's path prefix.
func (m *Module) pathPrefix() string {
	return m.path
}

// Feature declares the product outcome implemented by this module. A module
// owns at most one feature; use child modules for independently inspectable
// outcomes.
//
//	app.NewModule("tasks").Feature(app.Feature{
//		ID: "tasks/manage", Name: "Task management",
//		Outcome: "Users can create, list, update and complete tasks",
//		Owner: "samples",
//	})
func (m *Module) Feature(feature Feature) *Module {
	if m.feature != nil {
		panic(fmt.Sprintf("app.Module.Feature: module %q already declares feature %q", m.name, m.feature.ID))
	}
	feature.provenance = callerDesignSource(1)
	m.feature = &feature
	return m
}

// EffectiveFeature returns the feature declared by this module or its closest
// ancestor. It is used only by build-time native contributors.
func (m *Module) EffectiveFeature() *Feature {
	if m.feature != nil {
		return m.feature
	}
	if m.parent != nil {
		return m.parent.EffectiveFeature()
	}
	return nil
}

// FullPath returns the concatenated path from root to this module.
func (m *Module) FullPath() string {
	if m.parent != nil {
		parentPath := m.parent.FullPath()
		if parentPath != "" {
			return parentPath + m.path
		}
	}
	return m.path
}

// Root walks the parent chain and returns the top-most module. For a
// root module, Root returns the receiver. Useful when a plugin needs to
// discover capabilities across the whole application from inside its
// Configure phase (e.g. health endpoints aggregating probes).
func (m *Module) Root() *Module {
	root := m
	for root.parent != nil {
		root = root.parent
	}
	return root
}

// Provide registers a DI provider in this module.
func (m *Module) Provide(reg inject.Registration) *Module {
	m.registrations = append(m.registrations, reg)
	return m
}

// Use adds a plugin or sub-module to this module.
func (m *Module) Use(item any) *Module {
	switch v := item.(type) {
	case *Module:
		v.parent = m
		m.modules = append(m.modules, v)
	case Plugin:
		m.plugins = append(m.plugins, v)
	default:
		panic(fmt.Sprintf("app.Module.Use: unsupported type %T", item))
	}
	return m
}

// UseForDescribe registers a plugin that contributes build-time describe
// output WITHOUT taking part in the runtime lifecycle. Its Describe method
// runs during the describe phase (the `putnami build` describe pass), but the
// plugin is never auto-wired, configured, started, stopped, or added to the
// DI container — so it has no runtime cost and opens no connections.
//
// It is the describe-only dual of Use, for the common pattern of registering
// an infrastructure plugin conditionally on runtime config. The describe phase
// resolves a dev/test config (memory backends), so a plugin guarded by that
// config never registers and never describes — leaving committed
// infra/requirements.json and the aggregated .gen/requirements.json blind to
// the workload's real production needs. UseForDescribe lets the producer emit
// its infra scratch fragment from a build whose config does not activate the
// runtime plugin. The language generator then
// syncs the producer scratch fragment into committed infra/requirements.json,
// and the build aggregator includes it in .gen/requirements.json:
//
//	p := database.NewPlugin(prodConfig)
//	if cfg.UsesPostgres() {
//	    a.Use(p)            // full lifecycle in production
//	} else {
//	    a.UseForDescribe(p) // describe-only in dev/test builds
//	}
//
// A producer emits from its construction-time configuration. State that is
// only populated by the runtime lifecycle — e.g. database schemas attributed
// from migration sources collected during Configure — is not available in
// describe-only mode; declare such inputs on the plugin's config instead.
func (m *Module) UseForDescribe(d Describer) *Module {
	if d != nil {
		m.describeOnly = append(m.describeOnly, d)
	}
	return m
}

// OnPreConfigure registers a hook run before the DI container is built, in
// top-down order (root module first, then descendants). The DI container is
// not yet available — use this for work that must happen before any plugin is
// configured (e.g. deciding whether the module participates at all).
//
// Cleanup: a PreConfigure hook owns the teardown of anything it allocates
// until the application reaches the running state. If a later startup phase
// fails before Start completes, OnStop does NOT run (the app was never
// running), so a hook that opens a resource here must release it itself on a
// subsequent error — or defer the allocation to OnPostConfigure/OnStart, where
// OnStop pairs with it.
func (m *Module) OnPreConfigure(hook func(ctx context.Context) error) *Module {
	m.preConfigureHooks = append(m.preConfigureHooks, hook)
	return m
}

// OnPostConfigure registers a hook run after all plugins have configured, in
// bottom-up order (descendants first, then the root). The DI container is
// passed in so the hook can resolve services its subtree provides. Use this to
// coordinate across the plugins a module owns once they are all wired.
func (m *Module) OnPostConfigure(hook func(ctx context.Context, cc *inject.ContainerContext) error) *Module {
	m.postConfigureHooks = append(m.postConfigureHooks, hook)
	return m
}

// OnStart registers a hook run after the module's plugins have started, in
// top-down order. Use this to coordinate startup that depends on a module's
// plugins already running (e.g. a worker pool that waits for its dependencies).
//
// If an OnStart hook fails, the framework aborts startup and runs the full
// shutdown sequence (OnStop + plugin Stop) across every module — including
// modules whose OnStart had not yet run. A failing plugin Start (or a
// start-phase timeout) aborts startup the same way, running the full shutdown
// sequence rather than leaving the app half-started. Pair OnStart/OnStop
// carefully: the OnStop side must tolerate being called when its OnStart never
// executed (see OnStop).
func (m *Module) OnStart(hook func(ctx context.Context) error) *Module {
	m.startHooks = append(m.startHooks, hook)
	return m
}

// OnStop registers a shutdown hook called during application stop, before the
// module's plugins are stopped, in bottom-up order (descendants first).
//
// An OnStop hook must be defensive: it can be invoked during cleanup of a
// failed startup, so the matching OnStart (or PreConfigure/PostConfigure) may
// never have run. Guard against nil/zero state and make teardown idempotent
// rather than assuming a successful init — the same discipline as a Close that
// must tolerate a failed Open.
func (m *Module) OnStop(hook func(ctx context.Context) error) *Module {
	m.shutdownHooks = append(m.shutdownHooks, hook)
	return m
}

// Container returns the DI container context, available after the DI Build phase.
// Use this in Configure() for explicit resolution; plugin fields are also
// auto-wired from the container during the DI Build phase.
//
//	func (p *MyPlugin) Configure(ctx context.Context, owner *app.Module) error {
//	    svc, err := inject.Resolve[*MyService](owner.Container())
//	    ...
//	}
func (m *Module) Container() *inject.ContainerContext {
	return m.cc
}

// GetRegistrations returns all DI registrations for this module.
func (m *Module) GetRegistrations() []inject.Registration {
	return m.registrations
}

// CollectPlugins returns all plugins from this module and all sub-modules,
// paired with their owning module.
func (m *Module) CollectPlugins() []PluginOwner {
	result := make([]PluginOwner, 0, len(m.plugins))
	for _, p := range m.plugins {
		result = append(result, PluginOwner{Plugin: p, Owner: m})
	}
	for _, child := range m.modules {
		result = append(result, child.CollectPlugins()...)
	}
	return result
}

// collectDescribeOnly returns the describe-only contributors registered with
// UseForDescribe on this module and all sub-modules, in depth-first pre-order.
//
// These are deliberately absent from CollectPlugins and every Collect[T]
// capability walk: they must never be discovered as Configurers, Starters,
// HealthCheckers, MigrationContributors, or any other lifecycle role. Only the
// describe phase consults them.
func (m *Module) collectDescribeOnly() []Describer {
	result := append([]Describer(nil), m.describeOnly...)
	for _, child := range m.modules {
		result = append(result, child.collectDescribeOnly()...)
	}
	return result
}

// CollectModules returns this module and all descendant modules.
func (m *Module) CollectModules() []*Module {
	result := []*Module{m} //nolint:prealloc // size unknown (recursive)
	for _, child := range m.modules {
		result = append(result, child.CollectModules()...)
	}
	return result
}

// runPreConfigureHooks runs this module's PreConfigure hooks in registration
// order. Called by the App orchestrator top-down across the module tree.
func (m *Module) runPreConfigureHooks(ctx context.Context) error {
	for _, hook := range m.preConfigureHooks {
		if err := hook(ctx); err != nil {
			return err
		}
	}
	return nil
}

// runPostConfigureHooks runs this module's PostConfigure hooks in registration
// order, passing the module's DI container. Called by the App orchestrator
// bottom-up across the module tree.
func (m *Module) runPostConfigureHooks(ctx context.Context) error {
	for _, hook := range m.postConfigureHooks {
		if err := hook(ctx, m.cc); err != nil {
			return err
		}
	}
	return nil
}

// runStartHooks runs this module's OnStart hooks in registration order.
// Called by the App orchestrator top-down, after the module's plugins start.
func (m *Module) runStartHooks(ctx context.Context) error {
	for _, hook := range m.startHooks {
		if err := hook(ctx); err != nil {
			return err
		}
	}
	return nil
}

// runStopHooks runs this module's OnStop hooks in reverse registration order,
// collecting every error. Called by the App orchestrator bottom-up, before the
// module's plugins stop.
func (m *Module) runStopHooks(ctx context.Context) []error {
	var errs []error
	for i := len(m.shutdownHooks) - 1; i >= 0; i-- {
		if err := m.shutdownHooks[i](ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// PluginOwner pairs a plugin with the module that owns it.
type PluginOwner struct {
	Plugin Plugin
	Owner  *Module
}
