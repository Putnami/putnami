package app

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"syscall"
	"testing"
	"time"

	"go.putnami.dev/inject"
	"go.putnami.dev/protocol/features/spectest"
)

// --- Module Tests ---

func TestModuleName(t *testing.T) {
	m := NewModule("users")
	if m.Name() != "users" {
		t.Errorf("expected 'users', got %q", m.Name())
	}
}

func TestModulePath(t *testing.T) {
	m := NewModule("api").Path("/api/v1")
	if m.pathPrefix() != "/api/v1" {
		t.Errorf("expected '/api/v1', got %q", m.pathPrefix())
	}
}

func TestModuleFullPath(t *testing.T) {
	root := NewModule("root").Path("/api")
	child := NewModule("users").Path("/users")
	root.Use(child)

	if child.FullPath() != "/api/users" {
		t.Errorf("expected '/api/users', got %q", child.FullPath())
	}
}

func TestModuleProvide(t *testing.T) {
	token := inject.Named[string]("test")
	m := NewModule("test").Provide(inject.ProvideValue(token, "val"))
	if len(m.GetRegistrations()) != 1 {
		t.Errorf("expected 1 registration, got %d", len(m.GetRegistrations()))
	}
}

func TestModuleCollectPlugins(t *testing.T) {
	p1 := &testPlugin{name: "p1"}
	p2 := &testPlugin{name: "p2"}
	child := NewModule("child").Use(p2)
	root := NewModule("root").Use(p1).Use(child)

	plugins := root.CollectPlugins()
	if len(plugins) != 2 {
		t.Errorf("expected 2 plugins, got %d", len(plugins))
	}
	if plugins[0].Plugin.Name() != "p1" {
		t.Error("first plugin should be p1")
	}
	if plugins[1].Plugin.Name() != "p2" {
		t.Error("second plugin should be p2")
	}
}

func TestModuleCollectModules(t *testing.T) {
	child1 := NewModule("child1")
	child2 := NewModule("child2")
	root := NewModule("root").Use(child1).Use(child2)

	modules := root.CollectModules()
	if len(modules) != 3 {
		t.Errorf("expected 3 modules (root + 2 children), got %d", len(modules))
	}
}

func TestModuleShutdownHooks(t *testing.T) {
	called := false
	m := NewModule("test").OnStop(func(_ context.Context) error {
		called = true
		return nil
	})

	if errs := m.runStopHooks(context.Background()); len(errs) != 0 {
		t.Fatalf("expected no errors, got %v", errs)
	}
	if !called {
		t.Error("shutdown hook should have been called")
	}
}

// --- Application Tests ---

func TestApplicationLifecycle(t *testing.T) {
	warmupCalled := false
	startCalled := false
	stopCalled := false

	plugin := &lifecyclePlugin{
		onConfigure: func() error { warmupCalled = true; return nil },
		onStart:     func() error { startCalled = true; return nil },
		onStop:      func() error { stopCalled = true; return nil },
	}

	a := New("test-app")
	a.Use(plugin)

	a.Run(func(ctx context.Context) error {
		// Verify plugins were called
		if !warmupCalled {
			t.Error("warmup should have been called")
		}
		if !startCalled {
			t.Error("start should have been called")
		}
		return nil
	})

	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	if err := a.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}

	if !stopCalled {
		t.Error("stop should have been called")
	}
}

func TestApplicationDI(t *testing.T) {
	token := inject.Named[string]("greeting")

	a := New("di-test")
	a.Provide(inject.ProvideValue(token, "hello"))

	a.Run(func(ctx context.Context) error {
		val, err := a.Context().Get(token)
		if err != nil {
			t.Fatal(err)
		}
		if val.(string) != "hello" {
			t.Errorf("expected 'hello', got %v", val)
		}
		return nil
	})

	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer a.Stop(context.Background())
}

func TestApplicationIsRunning(t *testing.T) {
	a := New("running-test")
	a.Run(func(ctx context.Context) error { return nil })

	if a.IsRunning() {
		t.Error("should not be running before start")
	}

	a.Start(context.Background())
	// After Start with a runner that returns, the app is still "running"
	// until Stop is called
	if !a.IsRunning() {
		t.Error("should be running after start")
	}

	a.Stop(context.Background())
	if a.IsRunning() {
		t.Error("should not be running after stop")
	}
}

// --- DI bootstrap: a container is always created ---
//
// The framework registers a *migration.Registry into the container at
// Build time, so every Putnami application has a non-nil DI context even
// when no user-side providers are declared.

func TestApplicationWithoutUserProviders(t *testing.T) {
	warmupCalled := false
	startCalled := false

	a := New("no-di-app")
	a.Use(&lifecyclePlugin{
		onConfigure: func() error { warmupCalled = true; return nil },
		onStart:     func() error { startCalled = true; return nil },
	})

	a.Run(func(ctx context.Context) error {
		// The framework always builds a container — the *migration.Registry
		// is registered unconditionally.
		if a.Context() == nil {
			t.Error("expected non-nil context: framework registers *migration.Registry by default")
		}
		return nil
	})

	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer a.Stop(context.Background())

	if !warmupCalled {
		t.Error("warmup should be called even without DI")
	}
	if !startCalled {
		t.Error("start should be called even without DI")
	}
}

// --- Constructor-based DI (fx-style) ---

type testDB struct {
	DSN string
}

type testUserService struct {
	DB *testDB
}

func newTestDB() *testDB {
	return &testDB{DSN: "localhost:5432"}
}

func newTestUserService(db *testDB) *testUserService {
	return &testUserService{DB: db}
}

func TestApplicationProvideFunc(t *testing.T) {
	a := New("provide-func-test")
	a.ProvideFunc(newTestDB, newTestUserService)

	a.Run(func(ctx context.Context) error {
		cc := a.Context()
		if cc == nil {
			t.Fatal("expected DI context with ProvideFunc")
		}

		token := inject.TokenOf[*testUserService]()
		val, err := cc.Get(token)
		if err != nil {
			t.Fatal(err)
		}
		svc := val.(*testUserService)
		if svc.DB == nil {
			t.Fatal("expected DB to be injected")
		}
		if svc.DB.DSN != "localhost:5432" {
			t.Errorf("expected 'localhost:5432', got %q", svc.DB.DSN)
		}
		return nil
	})

	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer a.Stop(context.Background())
}

func TestApplicationInvokeFunc(t *testing.T) {
	invoked := false
	var resolvedDB *testDB

	a := New("invoke-test")
	a.ProvideFunc(newTestDB)
	a.InvokeFunc(func(db *testDB) {
		invoked = true
		resolvedDB = db
	})

	a.Run(func(ctx context.Context) error {
		if !invoked {
			t.Error("invoker should have been called before runner")
		}
		if resolvedDB == nil {
			t.Error("invoker should have received the DB")
		}
		return nil
	})

	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer a.Stop(context.Background())
}

func TestApplicationInvokeFuncWithError(t *testing.T) {
	a := New("invoke-error-test")
	a.ProvideFunc(newTestDB)
	a.InvokeFunc(func(db *testDB) error {
		return fmt.Errorf("setup failed")
	})

	err := a.Start(context.Background())
	if err == nil {
		t.Fatal("expected error from invoker")
	}
	if !contains(err.Error(), "setup failed") {
		t.Errorf("expected 'setup failed' in error, got %q", err.Error())
	}
}

// --- Mixed token-based and constructor-based ---

func TestApplicationMixedDI(t *testing.T) {
	a := New("mixed-test")

	// Token-based
	nameToken := inject.Named[string]("app-name")
	a.Provide(inject.ProvideValue(nameToken, "my-service"))

	// Constructor-based
	a.ProvideFunc(newTestDB)

	a.Run(func(ctx context.Context) error {
		cc := a.Context()

		// Token-based resolution
		name, err := cc.Get(nameToken)
		if err != nil {
			t.Fatal(err)
		}
		if name.(string) != "my-service" {
			t.Errorf("expected 'my-service', got %v", name)
		}

		// Constructor-based resolution
		dbToken := inject.TokenOf[*testDB]()
		db, err := cc.Get(dbToken)
		if err != nil {
			t.Fatal(err)
		}
		if db.(*testDB).DSN != "localhost:5432" {
			t.Errorf("expected 'localhost:5432', got %v", db.(*testDB).DSN)
		}

		return nil
	})

	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer a.Stop(context.Background())
}

func TestApplicationAlreadyRunning(t *testing.T) {
	a := New("already-running-test")
	a.Run(func(ctx context.Context) error { return nil })

	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	err := a.Start(context.Background())
	if err == nil {
		t.Fatal("expected error when starting already running app")
	}
	a.Stop(context.Background())
}

func TestApplicationStartTimeout(t *testing.T) {
	a := New("timeout-test")
	a.WithStartTimeout(50 * time.Millisecond)
	a.Use(&lifecyclePlugin{
		onStart: func() error {
			time.Sleep(500 * time.Millisecond)
			return nil
		},
	})
	a.Run(func(ctx context.Context) error { return nil })

	err := a.Start(context.Background())
	if err == nil {
		t.Fatal("expected start timeout error")
	}
}

func TestApplicationPluginStartFailure(t *testing.T) {
	stopCalled := false
	a := New("start-fail-test")
	a.Use(&lifecyclePlugin{
		onStart: func() error { return fmt.Errorf("plugin crashed") },
		onStop:  func() error { stopCalled = true; return nil },
	})
	a.Run(func(ctx context.Context) error { return nil })

	err := a.Start(context.Background())
	if err == nil {
		t.Fatal("expected error from plugin start failure")
	}
	if !stopCalled {
		t.Error("Stop should be called for cleanup after start failure")
	}
}

func TestApplicationConfigureFailure(t *testing.T) {
	a := New("warmup-fail-test")
	a.Use(&lifecyclePlugin{
		onConfigure: func() error { return fmt.Errorf("warmup failed") },
	})
	a.Run(func(ctx context.Context) error { return nil })

	err := a.Start(context.Background())
	if err == nil {
		t.Fatal("expected warmup error")
	}
}

func TestApplicationStopWhenNotRunning(t *testing.T) {
	a := New("stop-not-running")
	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("Stop on non-running app should return nil, got: %v", err)
	}
}

func TestApplicationRunnerError(t *testing.T) {
	a := New("runner-error-test")
	a.Run(func(ctx context.Context) error {
		return fmt.Errorf("runner failed")
	})

	err := a.Start(context.Background())
	if err == nil {
		t.Fatal("expected error from runner")
	}
	a.Stop(context.Background())
}

func TestListenAndServe_StopsAfterRunnerReturns(t *testing.T) {
	// A one-shot runner returns without a shutdown signal; ListenAndServe must
	// still run Stop so plugins/container are stopped and closed, per its
	// documented "Start + signal handling + Stop" contract.
	stopCalled := false
	a := New("listen-oneshot")
	a.Use(&lifecyclePlugin{
		onStop: func() error { stopCalled = true; return nil },
	})
	a.Run(func(_ context.Context) error { return nil })

	if err := a.ListenAndServe(); err != nil {
		t.Fatalf("ListenAndServe: %v", err)
	}
	if !stopCalled {
		t.Error("Stop was not run after the runner returned")
	}
	if a.IsRunning() {
		t.Error("application should not be running after ListenAndServe returns")
	}
}

func TestListenAndServe_StopsAfterRunnerError(t *testing.T) {
	// A runner that errors must also trigger Stop, and the runner error must be
	// the one returned.
	stopCalled := false
	a := New("listen-runner-err")
	a.Use(&lifecyclePlugin{
		onStop: func() error { stopCalled = true; return nil },
	})
	a.Run(func(_ context.Context) error { return fmt.Errorf("runner failed") })

	if err := a.ListenAndServe(); err == nil {
		t.Fatal("expected the runner error to propagate")
	}
	if !stopCalled {
		t.Error("Stop was not run after the runner errored")
	}
}

// startStopRacePlugin fires the shutdown signal from inside Start and then
// stays in-flight, touching a shared field that Stop also touches. If shutdown
// runs Stop while Start is still in-flight, -race reports a data race.
type startStopRacePlugin struct {
	shared  int
	stopped bool
}

func (p *startStopRacePlugin) Name() string { return "start-stop-race" }

func (p *startStopRacePlugin) Start(_ context.Context, _ *Module) error {
	// Deliver SIGTERM while this Start is still running. ListenAndServe has
	// already installed its signal handler (before Start), so it is captured.
	if self, err := os.FindProcess(os.Getpid()); err == nil {
		_ = self.Signal(syscall.SIGTERM)
	}
	for i := 0; i < 50; i++ {
		p.shared++
		time.Sleep(time.Millisecond)
	}
	return nil
}

func (p *startStopRacePlugin) Stop(_ context.Context, _ *Module) error {
	p.shared++ // same field as Start — a concurrent Stop makes -race fire
	p.stopped = true
	return nil
}

func TestListenAndServe_SignalDuringStartupNoRace(t *testing.T) {
	// A SIGTERM arriving during the start phase must not run Stop concurrently
	// with the in-flight Starter.Start. Run under -race to catch the regression.
	if runtime.GOOS == "windows" {
		t.Skip("a process cannot send itself SIGTERM on Windows")
	}
	p := &startStopRacePlugin{}
	a := New("listen-signal-start")
	a.Use(p)

	if err := a.ListenAndServe(); err != nil {
		t.Fatalf("ListenAndServe: %v", err)
	}
	if !p.stopped {
		t.Error("Stop should have run on signal-triggered shutdown")
	}
}

func TestModuleUsePanicOnUnsupportedType(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on unsupported type")
		}
	}()
	m := NewModule("test")
	m.Use("not a plugin or module")
}

func TestModuleFullPathNoParentPath(t *testing.T) {
	root := NewModule("root") // no path set
	child := NewModule("child").Path("/child")
	root.Use(child)

	if child.FullPath() != "/child" {
		t.Errorf("expected '/child', got %q", child.FullPath())
	}
}

func TestApplicationInvokerNonFunction(t *testing.T) {
	a := New("invoker-non-func")
	a.InvokeFunc("not a function")

	err := a.Start(context.Background())
	if err == nil {
		t.Fatal("expected error for non-function invoker")
	}
	a.Stop(context.Background())
}

func TestApplicationShutdownHookError(t *testing.T) {
	spectest.Proves(t, "go/application-lifecycle", "shutdown", "stop-hook-error-surfaces")
	a := New("hook-error-test")
	a.OnStop(func(_ context.Context) error { return fmt.Errorf("hook failed") })
	a.Run(func(ctx context.Context) error { return nil })

	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	err := a.Stop(context.Background())
	if err == nil {
		t.Fatal("expected error from shutdown hook")
	}
}

func TestApplicationPluginStopError(t *testing.T) {
	spectest.Proves(t, "go/application-lifecycle", "shutdown", "stop-plugin-error-surfaces")
	a := New("stop-error-test")
	a.Use(&lifecyclePlugin{
		onStop: func() error { return fmt.Errorf("stop failed") },
	})
	a.Run(func(ctx context.Context) error { return nil })

	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	err := a.Stop(context.Background())
	if err == nil {
		t.Fatal("expected error from plugin stop")
	}
}

// --- Test helpers ---

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsStr(s, substr))
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

type testPlugin struct {
	name string
}

func (p *testPlugin) Name() string { return p.name }

type lifecyclePlugin struct {
	onConfigure func() error
	onStart     func() error
	onStop      func() error
}

func (p *lifecyclePlugin) Name() string { return "lifecycle-test" }
func (p *lifecyclePlugin) Configure(_ context.Context, _ *Module) error {
	if p.onConfigure != nil {
		return p.onConfigure()
	}
	return nil
}
func (p *lifecyclePlugin) Start(_ context.Context, _ *Module) error {
	if p.onStart != nil {
		return p.onStart()
	}
	return nil
}
func (p *lifecyclePlugin) Stop(_ context.Context, _ *Module) error {
	if p.onStop != nil {
		return p.onStop()
	}
	return nil
}
