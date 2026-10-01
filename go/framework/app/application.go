package app

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.putnami.dev/errors"
	"go.putnami.dev/inject"
	"go.putnami.dev/logger"
	"go.putnami.dev/migration"
)

// DefaultStartTimeout is the maximum time allowed for the plugin start phase.
const DefaultStartTimeout = 30 * time.Second

// DefaultStopTimeout is the maximum time allowed for the graceful shutdown
// phase driven by ListenAndServe. It bounds Stop so one blocking Stopper/OnStop
// cannot hang the process forever, mirroring DefaultStartTimeout.
const DefaultStopTimeout = 30 * time.Second

// startDrainGrace bounds how long the start phase waits, after canceling the
// start context on timeout, for in-flight Start goroutines to observe the
// cancellation and return before Stop runs. Well-behaved Starters drain
// immediately; this cap prevents a Starter that ignores cancellation from
// re-hanging the timeout path indefinitely.
const startDrainGrace = 5 * time.Second

// Application is the root module and lifecycle orchestrator.
// It manages the DI container context and coordinates plugin phases.
//
// The framework always creates a DI container at Build time and registers
// a per-app *migration.Registry into it — this is what makes the migrate
// CLI self-collecting. User-side DI is layered on top: use ProvideFunc /
// InvokeFunc for constructor-based (fx-style) DI, or Provide for
// token-based DI. Both can be mixed freely.
type Application struct {
	*Module
	ctx                             *inject.ContainerContext
	log                             *logger.Logger
	running                         bool
	starting                        bool
	runner                          func(context.Context) error
	invokers                        []any // functions to call after DI is built
	startTimeout                    time.Duration
	stopTimeout                     time.Duration
	migrationRegistry               *migration.Registry
	capabilityMigrationAssociations []capabilityMigrationSourceAssociation
	mu                              sync.RWMutex
}

// New creates a new application with the given name.
//
//	app := app.New("my-service")
func New(name string) *Application {
	return &Application{
		Module: NewModule(name),
		log:    logger.Default().Named(name),
	}
}

// Run sets a custom runner function that executes after all plugins start.
// The runner receives a context that is canceled on shutdown.
func (a *Application) Run(runner func(ctx context.Context) error) *Application {
	a.runner = runner
	return a
}

// WithStartTimeout sets the maximum duration for the plugin start phase.
// If not set, DefaultStartTimeout (30s) is used.
func (a *Application) WithStartTimeout(d time.Duration) *Application {
	a.startTimeout = d
	return a
}

// WithStopTimeout sets the maximum duration for the graceful shutdown phase
// run by ListenAndServe. If not set, DefaultStopTimeout (30s) is used. This is
// the shutdown-side symmetric counterpart of WithStartTimeout, so a blocking
// Stopper/OnStop cannot hang the process indefinitely.
func (a *Application) WithStopTimeout(d time.Duration) *Application {
	a.stopTimeout = d
	return a
}

// newStopContext builds the bounded context for the shutdown phase, mirroring
// how the start phase derives its timeout.
func (a *Application) newStopContext() (context.Context, context.CancelFunc) {
	timeout := a.stopTimeout
	if timeout == 0 {
		timeout = DefaultStopTimeout
	}
	return context.WithTimeout(context.Background(), timeout)
}

// ProvideFunc registers a constructor-based provider (fx-style).
// The constructor's input parameters are resolved by type from the DI container,
// and the return value is registered by type.
//
// Supported signatures:
//
//	func() T
//	func() (T, error)
//	func(dep1 A, dep2 B) T
//	func(dep1 A, dep2 B) (T, error)
//
// Example:
//
//	app.ProvideFunc(NewUserService)  // func(db *sql.DB) *UserService
func (a *Application) ProvideFunc(constructors ...any) *Application {
	for _, c := range constructors {
		a.Provide(inject.AutoProvide(c))
	}
	return a
}

// ProvideScopedFunc registers a scoped constructor-based provider (fx-style).
// A new instance is created per scope (e.g., per HTTP request).
//
// Example:
//
//	app.ProvideScopedFunc(NewUserRepository)  // func(db *sql.DB) *UserRepository
func (a *Application) ProvideScopedFunc(constructors ...any) *Application {
	for _, c := range constructors {
		a.Provide(inject.AutoProvideScoped(c))
	}
	return a
}

// ProvideInstance registers a pre-built value in the DI container, keyed by its type.
// This is a shorthand for a.Provide(inject.ProvideInstance(value)).
//
// Example:
//
//	app.ProvideInstance(logger)    // registered as *logger.Logger
//	app.ProvideInstance(server)    // registered as *http.ServerPlugin
func (a *Application) ProvideInstance(values ...any) *Application {
	for _, v := range values {
		a.Provide(inject.ProvideInstanceOf(v))
	}
	return a
}

// InvokeFunc registers a function to be called after the DI container is built.
// The function's parameters are resolved from the container.
// Use this to kick off side effects (e.g., register routes, start workers).
//
// Example:
//
//	app.InvokeFunc(func(server *http.Server, users *UserService) {
//	    server.POST("/users", createUserHandler(users))
//	})
func (a *Application) InvokeFunc(fns ...any) *Application {
	a.invokers = append(a.invokers, fns...)
	return a
}

// Context returns the DI container context. The framework always builds a
// container (it registers a per-app *migration.Registry), so this is non-nil
// between Start and Stop regardless of whether any providers were registered.
// It is nil before Start and after Stop.
func (a *Application) Context() *inject.ContainerContext {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.ctx
}

// IsRunning returns true if the application is currently running.
func (a *Application) IsRunning() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.running
}

// Start runs the full application lifecycle: DI build -> configure ->
// migrate -> invoke -> start -> run. It blocks until the runner completes
// or a shutdown signal is received.
func (a *Application) Start(ctx context.Context) error {
	// Atomic double-start guard: under a single lock, reject if the app is
	// already running OR a concurrent Start is in flight, otherwise claim the
	// startup by setting `starting`. This closes the check-then-set window
	// where two concurrent Start calls on a fresh app both observed
	// running==false and both proceeded.
	a.mu.Lock()
	if a.running || a.starting {
		a.mu.Unlock()
		return errors.Newf(CodeAlreadyRunning, "application %q is already running", a.name)
	}
	a.starting = true
	a.mu.Unlock()

	// Release the startup claim on every exit path. On success `running` is
	// set to true below (so IsRunning observes it); here we only clear the
	// in-flight flag. On failure this resets the app so a later Start can
	// retry. IsRunning() reads `running`, not `starting`, so its semantics are
	// unchanged: it is true only after a successful startup.
	defer func() {
		a.mu.Lock()
		a.starting = false
		a.mu.Unlock()
	}()

	startedAt := time.Now()
	allPlugins := a.CollectPlugins()

	// Phase 1: Modules.PreConfigure (top-down), before the DI container exists.
	if err := a.runModulePreConfigure(ctx); err != nil {
		return err
	}

	// Phase 2: DI graph build.
	if err := a.buildContainer(allPlugins, true); err != nil {
		return err
	}

	// Phase 3: Plugins.Configure (flat walk).
	if err := a.configurePlugins(ctx, allPlugins); err != nil {
		return err
	}

	// Phase 4: Modules.PostConfigure (bottom-up), DI container ready.
	if err := a.runModulePostConfigure(ctx); err != nil {
		return err
	}

	if err := a.collectMigrationSources(); err != nil {
		a.closeContainer()
		return err
	}

	a.log.Debug("🔥 configured", slog.Int64("durationMs", time.Since(startedAt).Milliseconds()))

	// Phase 5: Migrate. The automatic lifecycle apply tolerates
	// source-only migration metadata so feature plugins can be reused in
	// apps that do not own every declared backend.
	if err := a.runMigrations(ctx, migration.ApplyOpts{AllowSourceOnly: true}); err != nil {
		a.closeContainer()
		return errors.Wrap(err, CodeConfigure)
	}

	if a.ctx != nil {
		if err := a.runInvokers(a.ctx); err != nil {
			a.closeContainer()
			return errors.Wrap(err, CodeInvoke)
		}
	}

	a.mu.Lock()
	a.running = true
	a.mu.Unlock()

	// Phase 6: Plugins.Start.
	if err := a.startPlugins(ctx, allPlugins); err != nil {
		return err
	}

	// Phase 7: Modules.OnStart (top-down), after their plugins.
	if err := a.runModuleStart(ctx); err != nil {
		// Cleanup uses a fresh context: the start ctx may be canceled, and a
		// canceled ctx would abort the very graceful shutdown we want here.
		if stopErr := a.Stop(context.Background()); stopErr != nil {
			a.log.Error("cleanup after module start failure", stopErr)
		}
		return err
	}

	a.log.Info("🤖 ready", slog.Int64("durationMs", time.Since(startedAt).Milliseconds()))

	if a.runner != nil {
		if err := a.runner(ctx); err != nil && ctx.Err() == nil {
			return errors.Wrap(err, CodeRunner)
		}
	} else {
		<-ctx.Done()
	}

	return nil
}

// buildContainer always creates the DI container, propagates it to modules,
// and auto-wires plugin fields. A fresh *migration.Registry is unconditionally
// provided into the container so every MigrationContributor + Runner can
// resolve the same instance; this is why the container is built even when the
// application registers no providers of its own.
//
// When eager is false the container is started without resolving non-lazy
// singletons — the describe phase uses this so harvesting metadata never
// reaches external systems. The runtime lifecycle (Start/Validate/Prepare)
// passes eager=true to keep fail-fast singleton construction.
func (a *Application) buildContainer(allPlugins []PluginOwner, eager bool) error {
	allRegs := a.collectAllRegistrations(allPlugins)

	// Build the per-app migration registry and publish it under the lock that
	// MigrationRegistry() reads with, so the writer and reader agree without a
	// data race. This is a standalone locked section (not nested in the
	// a.ctx-publishing lock below) because a.mu is a non-reentrant RWMutex.
	reg := migration.NewRegistry()
	a.mu.Lock()
	a.migrationRegistry = reg
	a.capabilityMigrationAssociations = nil
	a.mu.Unlock()
	allRegs = append(allRegs, inject.ProvideValue(
		inject.TokenOf[*migration.Registry](),
		reg,
	))

	a.log.Debug("building DI container")
	cc := inject.NewContainerContext(a.name)

	for _, reg := range allRegs {
		if err := cc.Register(reg); err != nil {
			return errors.Wrap(err, CodeRegister)
		}
	}

	start := cc.Start
	if !eager {
		start = cc.StartLazy
	}
	if err := start(); err != nil {
		return errors.Wrapf(err, CodeStart, "container start")
	}

	a.mu.Lock()
	a.ctx = cc
	a.mu.Unlock()

	for _, mod := range a.CollectModules() {
		mod.cc = cc
	}

	for _, po := range allPlugins {
		if err := inject.Wire(cc, po.Plugin); err != nil {
			a.closeContainer()
			return errors.Wrapf(err, CodeConfigure, "auto-wire failed", errors.String("plugin", po.Plugin.Name()))
		}
	}

	return nil
}

// collectMigrationSources walks every plugin in allPlugins and, for each
// MigrationContributor, AddSources its declared migration.Source values
// into the per-app registry. This is what makes the migrate CLI
// "self-collecting": adding a feature plugin to the app automatically
// widens the registry on the next boot, with no per-feature wiring in
// cmd/migrate.
func (a *Application) collectMigrationSources() error {
	if a.migrationRegistry == nil {
		return nil
	}
	for _, owned := range collectWithOwner[MigrationContributor](a.Module) {
		c := owned.Value
		for _, src := range c.MigrationSources() {
			if err := a.migrationRegistry.AddSource(src); err != nil {
				return errors.Wrapf(err, CodeConfigure, "collect migration sources",
					errors.String("plugin", c.Name()))
			}
			a.capabilityMigrationAssociations = append(a.capabilityMigrationAssociations, capabilityMigrationSourceAssociation{
				contributor: c,
				source:      src,
				owner:       owned.Owner,
			})
		}
	}
	return nil
}

// runMigrations applies pending migrations across every registered kind.
// Called by Start (with opts.Force == false so per-runner AutoApply gating is
// honored and opts.AllowSourceOnly == true so metadata-only sources are not
// fatal) and by Migrate (with whatever opts the caller provides). The migrate
// CLI bypasses Start entirely; it uses
// MigrationRegistry() and dispatches per-subcommand.
func (a *Application) runMigrations(ctx context.Context, opts migration.ApplyOpts) error {
	if a.migrationRegistry == nil {
		return nil
	}
	_, err := a.migrationRegistry.ApplyAll(ctx, opts)
	return err
}

// configurePlugins runs the Configure phase sequentially on all plugins.
func (a *Application) configurePlugins(ctx context.Context, allPlugins []PluginOwner) error {
	a.log.Debug("configure phase")
	for _, po := range allPlugins {
		if c, ok := po.Plugin.(Configurer); ok {
			if err := c.Configure(ctx, po.Owner); err != nil {
				a.closeContainer()
				return errors.Wrapf(err, CodeConfigure, "configure failed", errors.String("plugin", po.Plugin.Name()))
			}
		}
	}
	return nil
}

// runModulePreConfigure runs every module's PreConfigure hooks top-down
// (root → leaves), before the DI container is built.
func (a *Application) runModulePreConfigure(ctx context.Context) error {
	a.log.Debug("module pre-configure phase")
	for _, mod := range a.CollectModules() {
		if err := mod.runPreConfigureHooks(ctx); err != nil {
			return errors.Wrapf(err, CodeConfigure, "module pre-configure failed", errors.String("module", mod.Name()))
		}
	}
	return nil
}

// runModulePostConfigure runs every module's PostConfigure hooks bottom-up
// (leaves → root), after all plugins have configured. The DI container is
// available; this is closed by the caller on failure.
func (a *Application) runModulePostConfigure(ctx context.Context) error {
	a.log.Debug("module post-configure phase")
	mods := a.CollectModules()
	for i := len(mods) - 1; i >= 0; i-- {
		if err := mods[i].runPostConfigureHooks(ctx); err != nil {
			a.closeContainer()
			return errors.Wrapf(err, CodeConfigure, "module post-configure failed", errors.String("module", mods[i].Name()))
		}
	}
	return nil
}

// runModuleStart runs every module's OnStart hooks top-down (root → leaves),
// after the module's plugins have started.
func (a *Application) runModuleStart(ctx context.Context) error {
	a.log.Debug("module start phase")
	for _, mod := range a.CollectModules() {
		if err := mod.runStartHooks(ctx); err != nil {
			return errors.Wrapf(err, CodeStart, "module start failed", errors.String("module", mod.Name()))
		}
	}
	return nil
}

// startPlugins starts all Starter plugins in parallel with a timeout.
func (a *Application) startPlugins(ctx context.Context, allPlugins []PluginOwner) error {
	a.log.Debug("start phase")
	var startErrs []error
	var startMu sync.Mutex
	var wg sync.WaitGroup

	timeout := a.startTimeout
	if timeout == 0 {
		timeout = DefaultStartTimeout
	}

	// Derive a cancelable, timeout-bound child of the application context for
	// the start phase. Each Starter observes startCtx (not the bare caller
	// ctx) so the timeout — or an explicit cancel below — propagates into a
	// blocked Start. The caller's ctx is the application-lifetime context and
	// must not be canceled here; only this child is.
	startCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for _, po := range allPlugins {
		if s, ok := po.Plugin.(Starter); ok {
			wg.Add(1)
			go func(s Starter, owner *Module) {
				defer wg.Done()
				if err := s.Start(startCtx, owner); err != nil {
					startMu.Lock()
					startErrs = append(startErrs, errors.Wrapf(err, CodeStart, "start failed", errors.String("plugin", s.Name())))
					startMu.Unlock()
				}
			}(s, po.Owner)
		}
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(timeout):
		// Cancel startCtx so Starters that honor cancellation unblock, then join
		// wg (bounded by startDrainGrace) before Stop runs.
		cancel()
		select {
		case <-done:
		case <-time.After(startDrainGrace):
			a.log.Error("start goroutines did not drain after timeout cancel", errors.Newf(CodeStart, "start drain grace %s elapsed", startDrainGrace))
		}
		if stopErr := a.Stop(context.Background()); stopErr != nil {
			a.log.Error("cleanup after start timeout", stopErr)
		}
		return errors.Newf(CodeStart, "plugin start phase timed out after %s", timeout)
	}

	if len(startErrs) > 0 {
		if stopErr := a.Stop(context.Background()); stopErr != nil {
			a.log.Error("cleanup after start failure", stopErr)
		}
		// Aggregate every failing plugin (mirroring Stop) instead of returning
		// only startErrs[0] — otherwise sibling Start failures are silently
		// dropped and which one survives depends on goroutine scheduling.
		return errors.NewAggregate("start errors", startErrs)
	}

	return nil
}

// Stop gracefully shuts down the application. Module OnStop hooks run first
// (bottom-up: leaves → root), then plugin Stop methods (reverse registration
// order), then the DI container is closed.
//
// ctx is the shutdown context, threaded into every OnStop hook and plugin
// Stop. Pass a context with a deadline to bound the graceful-drain window;
// plugins that own a drain (e.g. the http server) clamp their own
// ShutdownTimeout against it. Pass context.Background() for an unbounded
// shutdown. Do not pass a context that is already canceled — plugins derive
// their drain timeout from it, so a canceled ctx aborts graceful shutdown
// immediately.
func (a *Application) Stop(ctx context.Context) error {
	a.mu.Lock()
	if !a.running {
		a.mu.Unlock()
		return nil
	}
	a.running = false
	a.mu.Unlock()

	a.log.Debug("stopping application")

	var errs []error

	// Phase 8: Modules.OnStop (bottom-up: leaves → root).
	mods := a.CollectModules()
	for i := len(mods) - 1; i >= 0; i-- {
		errs = append(errs, mods[i].runStopHooks(ctx)...)
	}

	// Phase 9: Stop plugins in reverse registration order.
	allPlugins := a.CollectPlugins()
	for i := len(allPlugins) - 1; i >= 0; i-- {
		po := allPlugins[i]
		if s, ok := po.Plugin.(Stopper); ok {
			if err := s.Stop(ctx, po.Owner); err != nil {
				errs = append(errs, errors.Wrapf(err, CodeStop, "stop failed", errors.String("plugin", po.Plugin.Name())))
			}
		}
	}

	// Close DI container and detach it from every module so a later Start
	// (after Stop) does not observe a stale, closed container.
	a.mu.Lock()
	cc := a.ctx
	a.ctx = nil
	a.mu.Unlock()
	if cc != nil {
		if err := cc.Close(); err != nil {
			errs = append(errs, errors.Wrapf(err, CodeStop, "container close"))
		}
	}
	a.detachModuleContainers()

	a.log.Info("application stopped")

	if len(errs) > 0 {
		return errors.NewAggregate("shutdown errors", errs)
	}
	return nil
}

// Validate runs DI Build, Configure, and Invoke phases without starting
// servers or listeners and without applying migrations. Use this for
// fast startup validation in tests and CI.
//
// Validate is safe to call before Start — the PreConfigure, Configure, and
// PostConfigure phases are idempotent and will be re-run with the real
// container when Start is called. Migrations are NOT applied; use
// Migrate(ctx) for the same shape with migrations included.
//
//	if err := a.Validate(); err != nil {
//	    log.Fatal("startup validation failed:", err)
//	}
func (a *Application) Validate() error {
	ctx := context.Background()
	allPlugins := a.CollectPlugins()

	if err := a.runModulePreConfigure(ctx); err != nil {
		return err
	}

	if err := a.buildContainer(allPlugins, true); err != nil {
		return err
	}

	if err := a.configurePlugins(ctx, allPlugins); err != nil {
		return err
	}

	if err := a.runModulePostConfigure(ctx); err != nil {
		return err
	}

	if err := a.collectMigrationSources(); err != nil {
		a.closeContainer()
		return err
	}

	if a.ctx != nil {
		if err := a.runInvokers(a.ctx); err != nil {
			a.closeContainer()
			return errors.Wrap(err, CodeInvoke)
		}
	}

	// Clean up — Validate is non-destructive
	a.closeContainer()
	return nil
}

// Prepare runs DI Build, Configure, and migration-source collection but
// does NOT run Invoke, Migrate, or Start. After Prepare, the DI
// container is open and the per-app *migration.Registry has every
// contributed Source registered — the migrate CLI uses this state to
// dispatch subcommands (up, down, status, verify, inspect) against the
// registry. Callers must call Stop() to release the container.
//
// Prepare and Start are mutually exclusive on the same Application:
// Start runs Prepare internally as part of its own lifecycle.
func (a *Application) Prepare(ctx context.Context) error {
	allPlugins := a.CollectPlugins()

	if err := a.runModulePreConfigure(ctx); err != nil {
		return err
	}
	if err := a.buildContainer(allPlugins, true); err != nil {
		return err
	}
	if err := a.configurePlugins(ctx, allPlugins); err != nil {
		a.closeContainer()
		return err
	}
	if err := a.runModulePostConfigure(ctx); err != nil {
		return err
	}
	if err := a.collectMigrationSources(); err != nil {
		a.closeContainer()
		return err
	}
	return nil
}

// Migrate runs Prepare then applies pending migrations across every
// registered kind. ApplyOpts is forwarded to each runner. With the zero
// value, runners honor their own auto-apply settings (typically:
// AutoApply = true for samples and tests, false for production
// services that rely on a dedicated migrate job). Set opts.Force = true
// to override that gating — the migrate CLI passes Force on `up`.
//
// The container is closed when Migrate returns, regardless of outcome.
// Migrate and Start are mutually exclusive on the same Application.
func (a *Application) Migrate(ctx context.Context, opts migration.ApplyOpts) error {
	if err := a.Prepare(ctx); err != nil {
		return err
	}
	defer a.closeContainer()
	return a.runMigrations(ctx, opts)
}

// MigrationRegistry returns the per-application migration registry. Nil
// before Prepare/Start has run; non-nil after. The migrate CLI calls
// this after Prepare to dispatch subcommands directly against the
// registered runners.
func (a *Application) MigrationRegistry() *migration.Registry {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.migrationRegistry
}

// ListenAndServe starts the application and blocks until shutdown.
// Convenience method that wraps Start + signal handling + Stop.
//
// When the PUTNAMI_DESCRIBE environment variable is set, ListenAndServe
// runs Describe instead and returns without starting any servers — this
// is the build-time hook that extension generators rely on to extract
// proto/openapi/etc. from the configured plugin chain.
func (a *Application) ListenAndServe() error {
	if target := os.Getenv(describeEnvVar); target != "" {
		outDir := os.Getenv(describeOutEnvVar)
		if outDir == "" {
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			outDir = filepath.Join(cwd, ".gen")
		}
		return a.Describe(outDir, splitTargets(target))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		select {
		case <-sigCh:
			a.log.Debug("shutdown signal received")
			// Only cancel here. Stop runs solely on the return path below, after
			// Start has fully returned — so it never touches plugins while their
			// Starter.Start is still in-flight during startup (the Start/Stop
			// hazard the start-timeout path also guards).
			cancel()
		case <-ctx.Done():
			// Start returned on its own (one-shot/batch runner finished, or the
			// runner errored); this goroutine just exits instead of leaking
			// parked on sigCh.
		}
	}()

	startErr := a.Start(ctx)
	// Release the signal goroutine if no signal ever arrived, then always run
	// Stop — exactly once, from here.
	cancel()
	stopCtx, stopCancel := a.newStopContext()
	defer stopCancel()
	stopErr := a.Stop(stopCtx)

	if startErr != nil {
		return startErr
	}
	return stopErr
}

// splitTargets parses the PUTNAMI_DESCRIBE value. Comma- or space-delimited;
// trims whitespace; drops empties. "all" is preserved verbatim so callers
// can short-circuit on it via DescribeContext.Wants.
func splitTargets(raw string) []string {
	out := []string{}
	for _, sep := range []string{",", " "} {
		raw = strings.ReplaceAll(raw, sep, "\x00")
	}
	for t := range strings.SplitSeq(raw, "\x00") {
		t = strings.TrimSpace(t)
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}

// closeContainer closes the DI container if it was created. Used for cleanup
// when a lifecycle phase fails after the container was started.
func (a *Application) closeContainer() {
	a.mu.RLock()
	cc := a.ctx
	a.mu.RUnlock()
	if cc != nil {
		if err := cc.Close(); err != nil {
			a.log.Error("container close during cleanup", err)
		}
		a.mu.Lock()
		a.ctx = nil
		a.mu.Unlock()
	}
	a.detachModuleContainers()
}

// detachModuleContainers clears the DI container reference from every module.
// buildContainer attaches the live container to each module; without this
// matching teardown, Module.Container() would keep returning a closed
// container after Stop/Validate/Migrate, so a subsequent lifecycle pass (e.g.
// Start after Stop, or Start after Validate) could run OnPreConfigure — whose
// contract is that DI is unavailable — while observing a stale container.
func (a *Application) detachModuleContainers() {
	for _, mod := range a.CollectModules() {
		mod.cc = nil
	}
}

// collectAllRegistrations gathers DI registrations from all modules and Provider plugins.
func (a *Application) collectAllRegistrations(allPlugins []PluginOwner) []inject.Registration {
	var regs []inject.Registration
	for _, mod := range a.CollectModules() {
		regs = append(regs, mod.GetRegistrations()...)
	}
	for _, po := range allPlugins {
		if p, ok := po.Plugin.(Provider); ok {
			regs = append(regs, p.Provides()...)
		}
	}
	return regs
}

// runInvokers calls all registered invoker functions, resolving their params from the container.
func (a *Application) runInvokers(cc *inject.ContainerContext) error {
	for i, invoker := range a.invokers {
		fn := reflect.ValueOf(invoker)
		ft := fn.Type()

		if ft.Kind() != reflect.Func {
			return errors.Newf(CodeInvoke, "invoker %d: expected a function, got %T", i, invoker)
		}

		// Resolve all parameters
		args := make([]reflect.Value, ft.NumIn())
		for j := 0; j < ft.NumIn(); j++ {
			paramType := ft.In(j)
			token := inject.TokenOf2(paramType)
			val, err := cc.Get(token)
			if err != nil {
				return errors.Wrapf(err, CodeInvoke, "invoker param resolution failed",
					errors.Int("invoker", i), errors.Int("param", j), errors.String("type", paramType.String()))
			}
			// A nil-registered token resolves to (nil, nil); reflect.ValueOf(nil)
			// is the zero Value, which makes fn.Call panic. Substitute the typed
			// zero and guard assignability, mirroring the health-probe invoker.
			if val == nil {
				args[j] = reflect.Zero(paramType)
				continue
			}
			rv := reflect.ValueOf(val)
			if !rv.Type().AssignableTo(paramType) {
				return errors.Newf(CodeInvoke, "invoker %d param %d: type mismatch: got %s, want %s",
					i, j, rv.Type(), paramType)
			}
			args[j] = rv
		}

		results := fn.Call(args)

		// If the invoker returns an error, check it
		if len(results) > 0 {
			last := results[len(results)-1]
			if last.Type().Implements(reflect.TypeOf((*error)(nil)).Elem()) && !last.IsNil() {
				if e, ok := last.Interface().(error); ok {
					return errors.Wrapf(e, CodeInvoke, "invoker failed", errors.Int("invoker", i))
				}
			}
		}
	}
	return nil
}
