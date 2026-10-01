package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"go.putnami.dev/inject"
	"go.putnami.dev/protocol/features/spectest"
)

// --- ProvideScopedFunc contract ---

// scopedThing is constructed once per scope. Each instance gets a unique id so
// that distinct scopes are observably distinct even beyond pointer identity.
type scopedThing struct {
	id int64
}

// scopedThingSeq hands out a fresh id per construction.
var scopedThingSeq atomic.Int64

func newScopedThing() *scopedThing {
	return &scopedThing{id: scopedThingSeq.Add(1)}
}

// TestProvideScopedFunc_DistinctInstancesPerScope registers a scoped
// constructor and resolves it in two different scopes, asserting the scopes get
// distinct instances (a new instance per scope is the whole point of Scoped).
func TestProvideScopedFunc_DistinctInstancesPerScope(t *testing.T) {
	a := New("scoped-func")
	a.ProvideScopedFunc(newScopedThing)

	a.Run(func(ctx context.Context) error {
		cc := a.Context()
		if cc == nil {
			t.Fatal("expected DI context")
		}

		token := inject.TokenOf[*scopedThing]()

		scope1, err := cc.CreateScope()
		if err != nil {
			t.Fatalf("CreateScope #1: %v", err)
		}
		defer scope1.Close() //nolint:errcheck

		scope2, err := cc.CreateScope()
		if err != nil {
			t.Fatalf("CreateScope #2: %v", err)
		}
		defer scope2.Close() //nolint:errcheck

		v1, err := scope1.Get(token)
		if err != nil {
			t.Fatalf("resolve in scope #1: %v", err)
		}
		v2, err := scope2.Get(token)
		if err != nil {
			t.Fatalf("resolve in scope #2: %v", err)
		}

		t1 := v1.(*scopedThing)
		t2 := v2.(*scopedThing)

		if t1 == t2 {
			t.Error("scoped provider returned the same instance across two scopes")
		}
		if t1.id == t2.id {
			t.Errorf("scoped instances share id %d; expected distinct ids", t1.id)
		}

		// Resolving again within the same scope must return that scope's cached
		// instance, confirming per-scope (not per-call) lifetime.
		again, err := scope1.Get(token)
		if err != nil {
			t.Fatalf("re-resolve in scope #1: %v", err)
		}
		if again.(*scopedThing) != t1 {
			t.Error("scoped provider should cache one instance per scope")
		}
		return nil
	})

	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer a.Stop(context.Background()) //nolint:errcheck
}

// --- ProvideInstance contract ---

type providedInstance struct {
	name string
}

// TestProvideInstance_ReturnsSamePointer registers a pre-built value and
// asserts the container hands back the exact same pointer, keyed by the value's
// type.
func TestProvideInstance_ReturnsSamePointer(t *testing.T) {
	want := &providedInstance{name: "singleton"}

	a := New("provide-instance")
	a.ProvideInstance(want)

	a.Run(func(ctx context.Context) error {
		cc := a.Context()
		if cc == nil {
			t.Fatal("expected DI context")
		}

		got, err := cc.Get(inject.TokenOf[*providedInstance]())
		if err != nil {
			t.Fatalf("resolve provided instance: %v", err)
		}
		if got.(*providedInstance) != want {
			t.Errorf("ProvideInstance: got %p, want same pointer %p", got, want)
		}
		return nil
	})

	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer a.Stop(context.Background()) //nolint:errcheck
}

// --- Module.Root contract ---

// TestModuleRoot_WalksToTopOfTree builds a 3-level module tree and asserts a
// leaf's Root() returns the top module, and a root's Root() returns itself.
func TestModuleRoot_WalksToTopOfTree(t *testing.T) {
	root := NewModule("root")
	mid := NewModule("mid")
	leaf := NewModule("leaf")

	// root -> mid -> leaf
	root.Use(mid)
	mid.Use(leaf)

	if got := leaf.Root(); got != root {
		t.Errorf("leaf.Root() = %q (%p), want top module %q (%p)", got.Name(), got, root.Name(), root)
	}
	if got := mid.Root(); got != root {
		t.Errorf("mid.Root() = %q, want top module %q", got.Name(), root.Name())
	}
	if got := root.Root(); got != root {
		t.Errorf("root.Root() = %q (%p), want itself %q (%p)", got.Name(), got, root.Name(), root)
	}
}

// --- ListenAndServe describe-dispatch branch ---

// describeStarterPlugin is both a Describer and a Starter. In the describe
// branch of ListenAndServe, Describe must run and Start must NOT — proving no
// server is started.
type describeStarterPlugin struct {
	name        string
	artifact    string
	described   atomic.Bool
	startCalled atomic.Bool
}

func (p *describeStarterPlugin) Name() string { return p.name }

func (p *describeStarterPlugin) Describe(ctx *DescribeContext) error {
	p.described.Store(true)
	if !ctx.Wants(p.name) {
		return nil
	}
	path := filepath.Join(ctx.OutputDir, p.artifact)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	return os.WriteFile(path, []byte("ok"), 0o600)
}

func (p *describeStarterPlugin) Start(_ context.Context, _ *Module) error {
	p.startCalled.Store(true)
	return nil
}

// TestListenAndServe_DescribeBranchWritesArtifactsAndSkipsStart covers the
// PUTNAMI_DESCRIBE dispatch branch of ListenAndServe: with the env var set,
// ListenAndServe runs Describe, writes artifacts, and returns without starting
// any server (no signal handling, no Start phase).
func TestListenAndServe_DescribeBranchWritesArtifactsAndSkipsStart(t *testing.T) {
	spectest.Proves(t, "go/application-lifecycle", "describe", "describe-skips-runtime-start")
	outDir := t.TempDir()
	t.Setenv(describeEnvVar, "all")
	t.Setenv(describeOutEnvVar, outDir)

	p := &describeStarterPlugin{name: "openapi", artifact: "schema/openapi.json"}
	a := New("describe-dispatch")
	a.Use(p)

	if err := a.ListenAndServe(); err != nil {
		t.Fatalf("ListenAndServe (describe mode): %v", err)
	}

	if !p.described.Load() {
		t.Error("Describe should have run in describe mode")
	}
	if p.startCalled.Load() {
		t.Error("Start must NOT run in describe mode (no server should be started)")
	}
	if a.IsRunning() {
		t.Error("application must not be running after describe-mode ListenAndServe")
	}

	if _, err := os.Stat(filepath.Join(outDir, "schema/openapi.json")); err != nil {
		t.Errorf("expected describe artifact to be written: %v", err)
	}
}

// --- Describe must not eagerly resolve singletons ---

// externalResource stands in for a provider whose constructor reaches an
// external system (a DB pool, a remote client). Describe harvests metadata at
// build time and must never trigger such construction — the build host has no
// database. The constructor below fails loudly if it ever runs.
type externalResource struct{}

// TestDescribe_DoesNotResolveEagerSingletons proves the describe phase starts
// the DI container lazily: an eager (non-lazy) singleton whose constructor
// errors is registered, yet Describe succeeds because the constructor is never
// invoked. This keeps metadata builds independent from external systems that
// eager constructors may contact.
func TestDescribe_DoesNotResolveEagerSingletons(t *testing.T) {
	spectest.Proves(t, "go/application-lifecycle", "describe", "describe-defers-eager-providers")
	var built atomic.Bool

	a := New("describe-lazy")
	a.ProvideFunc(func() (*externalResource, error) {
		built.Store(true)
		return nil, errors.New("constructor must not run during describe")
	})

	if err := a.Describe(t.TempDir(), nil); err != nil {
		t.Fatalf("Describe must not resolve eager singletons: %v", err)
	}
	if built.Load() {
		t.Error("describe resolved an eager singleton; it must defer every non-lazy provider")
	}
}
