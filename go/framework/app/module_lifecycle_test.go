package app

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"go.putnami.dev/inject"
	"go.putnami.dev/protocol/features/spectest"
)

// recorder is a thread-safe ordered log of lifecycle events. Plugin Start runs
// in a goroutine, so appends must be guarded.
type recorder struct {
	mu     sync.Mutex
	events []string
}

func (r *recorder) add(event string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *recorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.events))
	copy(out, r.events)
	return out
}

// hookPlugin records its plugin-phase invocations into a shared recorder.
type hookPlugin struct {
	rec *recorder
}

func (p *hookPlugin) Name() string { return "hook-plugin" }
func (p *hookPlugin) Configure(_ context.Context, _ *Module) error {
	p.rec.add("cfg:plugin")
	return nil
}
func (p *hookPlugin) Start(_ context.Context, _ *Module) error {
	p.rec.add("start:plugin")
	return nil
}
func (p *hookPlugin) Stop(_ context.Context, _ *Module) error {
	p.rec.add("stop:plugin")
	return nil
}

// wireModule registers all four lifecycle hooks on m, tagging each event with
// the module's name so ordering across the tree is observable.
func wireModule(m *Module, rec *recorder, t *testing.T) {
	name := m.Name()
	m.OnPreConfigure(func(ctx context.Context) error {
		if ctx == nil {
			t.Errorf("%s: pre-configure ctx is nil", name)
		}
		rec.add("pre:" + name)
		return nil
	})
	m.OnPostConfigure(func(ctx context.Context, cc *inject.ContainerContext) error {
		if ctx == nil {
			t.Errorf("%s: post-configure ctx is nil", name)
		}
		if cc == nil {
			t.Errorf("%s: post-configure container is nil", name)
		}
		rec.add("post:" + name)
		return nil
	})
	m.OnStart(func(ctx context.Context) error {
		if ctx == nil {
			t.Errorf("%s: start ctx is nil", name)
		}
		rec.add("onstart:" + name)
		return nil
	})
	m.OnStop(func(ctx context.Context) error {
		if ctx == nil {
			t.Errorf("%s: stop ctx is nil", name)
		}
		rec.add("onstop:" + name)
		return nil
	})
}

// TestModuleLifecyclePhaseOrdering asserts the phase-major ordering documented
// on Module: PreConfigure (top-down) → plugin Configure → PostConfigure
// (bottom-up) → plugin Start → OnStart (top-down), then on shutdown OnStop
// (bottom-up) → plugin Stop.
func TestModuleLifecyclePhaseOrdering(t *testing.T) {
	spectest.Proves(t, "go/application-lifecycle", "phase-order", "startup-phase-order")
	spectest.Proves(t, "go/application-lifecycle", "shutdown", "shutdown-phase-order")
	rec := &recorder{}

	grand := NewModule("grand")
	child := NewModule("child").Use(grand)

	a := New("root")
	a.Use(child)
	a.Use(&hookPlugin{rec: rec})

	wireModule(a.Module, rec, t)
	wireModule(child, rec, t)
	wireModule(grand, rec, t)

	a.Run(func(_ context.Context) error { return nil })

	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}

	got := rec.snapshot()
	want := []string{
		// Phase 1: PreConfigure top-down (root → leaves).
		"pre:root", "pre:child", "pre:grand",
		// Phase 3: plugin Configure.
		"cfg:plugin",
		// Phase 4: PostConfigure bottom-up (leaves → root).
		"post:grand", "post:child", "post:root",
		// Phase 6: plugin Start.
		"start:plugin",
		// Phase 7: OnStart top-down.
		"onstart:root", "onstart:child", "onstart:grand",
		// Phase 8: OnStop bottom-up (leaves → root).
		"onstop:grand", "onstop:child", "onstop:root",
		// Phase 9: plugin Stop.
		"stop:plugin",
	}

	if len(got) != len(want) {
		t.Fatalf("event count mismatch:\n got=%v\nwant=%v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event[%d] = %q, want %q\nfull got=%v", i, got[i], want[i], got)
		}
	}
}

// TestModulePreConfigureRunsBeforeDIBuild verifies a PreConfigure hook executes
// before the DI container exists — the container is only attached at build time.
func TestModulePreConfigureRunsBeforeDIBuild(t *testing.T) {
	spectest.Proves(t, "go/application-lifecycle", "phase-order", "pre-configure-runs-before-di-build")
	var containerAtPreConfigure *inject.ContainerContext
	seen := false

	a := New("pre-di")
	a.OnPreConfigure(func(_ context.Context) error {
		seen = true
		containerAtPreConfigure = a.Module.cc
		return nil
	})
	a.Run(func(_ context.Context) error { return nil })

	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer a.Stop(context.Background())

	if !seen {
		t.Fatal("pre-configure hook was not called")
	}
	if containerAtPreConfigure != nil {
		t.Error("DI container should not be attached during pre-configure")
	}
}

// TestModuleHookErrorAbortsStartup verifies an error from a PostConfigure hook
// aborts startup and closes the container.
func TestModuleHookErrorAbortsStartup(t *testing.T) {
	a := New("hook-abort")
	a.OnPostConfigure(func(_ context.Context, _ *inject.ContainerContext) error {
		return errPostConfigure
	})
	a.Run(func(_ context.Context) error { return nil })

	err := a.Start(context.Background())
	if err == nil {
		t.Fatal("expected start to fail when a post-configure hook errors")
	}
	if a.Context() != nil {
		t.Error("container should be closed after post-configure failure")
	}
}

var errPostConfigure = fmt.Errorf("post-configure boom")

// ctxKey is a private type for the Stop-context propagation test.
type ctxKey struct{}

// TestStopContextPropagatesToOnStop verifies the context passed to Stop reaches
// OnStop hooks (and is not replaced by a synthesized background context).
func TestStopContextPropagatesToOnStop(t *testing.T) {
	var seen any
	a := New("stop-ctx")
	a.OnStop(func(ctx context.Context) error {
		seen = ctx.Value(ctxKey{})
		return nil
	})
	a.Run(func(_ context.Context) error { return nil })

	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}

	shutdownCtx := context.WithValue(context.Background(), ctxKey{}, "drain")
	if err := a.Stop(shutdownCtx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if seen != "drain" {
		t.Errorf("OnStop did not receive the Stop ctx value: got %v, want \"drain\"", seen)
	}
}

// TestModuleContainerDetachedAfterCleanup guards against a stale container
// leaking across lifecycle passes: after Validate (or Stop) closes the DI
// container, Module.Container() must read nil, so a later Start runs
// OnPreConfigure with DI genuinely unavailable rather than pointing at a
// closed container.
func TestModuleContainerDetachedAfterCleanup(t *testing.T) {
	spectest.Proves(t, "go/application-lifecycle", "shutdown", "container-detached-after-cleanup")
	a := New("detach")

	// Validate builds the container then tears it down.
	if err := a.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if a.Module.Container() != nil {
		t.Error("module container should be nil after Validate cleanup")
	}

	preConfigureSawContainer := false
	a.OnPreConfigure(func(_ context.Context) error {
		preConfigureSawContainer = a.Module.Container() != nil
		return nil
	})
	a.Run(func(_ context.Context) error { return nil })

	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if preConfigureSawContainer {
		t.Error("pre-configure observed a stale container left over from Validate")
	}

	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if a.Module.Container() != nil {
		t.Error("module container should be nil after Stop")
	}
}

// namedStopPlugin records its own Stop into a shared recorder so the relative
// order of sibling plugin shutdowns is observable.
type namedStopPlugin struct {
	name string
	rec  *recorder
}

func (p *namedStopPlugin) Name() string { return p.name }
func (p *namedStopPlugin) Stop(_ context.Context, _ *Module) error {
	p.rec.add("stop:" + p.name)
	return nil
}

// TestPluginsStopInReverseRegistrationOrder asserts shutdown unwinds the plugin
// list last-registered-first. A plugin registered later may depend on an
// earlier one still being up (a metrics exporter on its transport, a worker on
// its queue), so tearing down in registration order would pull the floor out
// from under it.
func TestPluginsStopInReverseRegistrationOrder(t *testing.T) {
	spectest.Proves(t, "go/application-lifecycle", "shutdown", "plugins-stop-in-reverse-registration-order")
	rec := &recorder{}

	a := New("reverse-stop")
	a.Use(&namedStopPlugin{name: "first", rec: rec})
	a.Use(&namedStopPlugin{name: "second", rec: rec})
	a.Use(&namedStopPlugin{name: "third", rec: rec})
	a.Run(func(_ context.Context) error { return nil })

	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}

	got := rec.snapshot()
	want := []string{"stop:third", "stop:second", "stop:first"}
	if len(got) != len(want) {
		t.Fatalf("stop event count mismatch:\n got=%v\nwant=%v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("stop event[%d] = %q, want %q\nfull got=%v", i, got[i], want[i], got)
		}
	}
}
