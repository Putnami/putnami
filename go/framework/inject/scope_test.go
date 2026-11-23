package inject

import (
	"context"
	"fmt"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

func TestScopeFrom_NoScope(t *testing.T) {
	scope := ScopeFrom(context.Background())
	if scope != nil {
		t.Error("expected nil scope from background context")
	}
}

func TestScopeFrom_WithScope(t *testing.T) {
	parent := NewContainer("test", nil)
	ctx := withScope(context.Background(), newDetachedScope(parent))
	scope := ScopeFrom(ctx)
	if scope == nil {
		t.Fatal("expected non-nil scope")
	}
	if scope.parent != parent {
		t.Error("the scope's container is not a child of the scope's parent")
	}
	if again := ScopeFrom(ctx); again != scope {
		t.Error("a second lookup returned another container")
	}
}

func TestResolve_NoScope(t *testing.T) {
	token := Named[string]("test")
	_, err := Resolve[string](context.Background(), token)
	if err == nil {
		t.Fatal("expected error resolving without scope")
	}
}

func TestResolve_TypeMismatch(t *testing.T) {
	cc := NewContainerContext("test")
	token := Named[int]("port")
	cc.Register(ProvideValue(token, 8080))
	if err := cc.Start(); err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	err := cc.Scope(context.Background(), func(ctx context.Context, _ ScopeContext) error {
		_, err := Resolve[string](ctx, token)
		if err == nil {
			return fmt.Errorf("expected type mismatch error")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestResolveFromContext_NoScope(t *testing.T) {
	token := Named[string]("test")
	_, err := ResolveFromContext(context.Background(), token)
	if err == nil {
		t.Fatal("expected error resolving without scope")
	}
}

func TestResolveFromContext_WithScope(t *testing.T) {
	cc := NewContainerContext("test")
	token := Named[string]("greeting")
	cc.Register(ProvideValue(token, "hello"))
	if err := cc.Start(); err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	err := cc.Scope(context.Background(), func(ctx context.Context, _ ScopeContext) error {
		val, err := ResolveFromContext(ctx, token)
		if err != nil {
			return err
		}
		if val != "hello" {
			return fmt.Errorf("got %v, want hello", val)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestContainerContext_Scope_ClosesOnReturn(t *testing.T) {
	cc := NewContainerContext("test")
	token := Named[string]("val")
	cc.Register(ProvideValue(token, "x"))
	if err := cc.Start(); err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	var scopeCtx context.Context
	err := cc.Scope(context.Background(), func(ctx context.Context, _ ScopeContext) error {
		scopeCtx = ctx
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Scope should be closed — resolving from it should still work
	// (the scope container is still in the context, just closed)
	scope := ScopeFrom(scopeCtx)
	if scope == nil {
		t.Fatal("scope still in context after close")
	}
}

func TestCreateScope_NotStarted(t *testing.T) {
	cc := NewContainerContext("test")
	_, err := cc.CreateScope()
	if err == nil {
		t.Fatal("expected error creating scope before start")
	}
}

func TestCreateScope_Detached(t *testing.T) {
	cc := NewContainerContext("test")
	token := Named[int]("port")
	cc.Register(ProvideValue(token, 8080))
	if err := cc.Start(); err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	scope, err := cc.CreateScope()
	if err != nil {
		t.Fatal(err)
	}

	ctx := scope.Context(context.Background())
	val, err := Resolve[int](ctx, token)
	if err != nil {
		t.Fatalf("resolve from detached scope: %v", err)
	}
	if val != 8080 {
		t.Errorf("got %d, want 8080", val)
	}

	if err := scope.Close(); err != nil {
		t.Fatalf("close scope: %v", err)
	}
}

func TestDetachedScopeCreatesItsContainerOnFirstResolution(t *testing.T) {
	cc := NewContainerContext("lazy-scope")
	tok := Named[int]("n")
	if err := cc.Register(ProvideValue(tok, 1)); err != nil {
		t.Fatal(err)
	}
	if err := cc.Start(); err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	unused, err := cc.CreateScope()
	if err != nil {
		t.Fatal(err)
	}
	_ = unused.Context(context.Background())
	if unused.materialized() != nil {
		t.Fatal("creating the scope and its context created a container")
	}
	if err := unused.Finalize(context.Background(), nil); err != nil {
		t.Fatalf("Finalize of an unused scope: %v", err)
	}
	if err := unused.Close(); err != nil {
		t.Fatalf("Close of an unused scope: %v", err)
	}
	if n := len(cc.root.children); n != 0 {
		t.Fatalf("the root holds %d children after an unused scope, want 0", n)
	}

	used, err := cc.CreateScope()
	if err != nil {
		t.Fatal(err)
	}
	ctx := used.Context(context.Background())
	if got, err := Resolve[int](ctx, tok); err != nil || got != 1 {
		t.Fatalf("Resolve = %v, %v, want 1", got, err)
	}
	if used.materialized() == nil {
		t.Fatal("a resolution did not create the container")
	}
	if err := used.Close(); err != nil {
		t.Fatal(err)
	}
	if n := len(cc.root.children); n != 0 {
		t.Fatalf("the root holds %d children after the scope closed, want 0", n)
	}
}

func TestDetachedScopeClosedBeforeUseRefusesResolution(t *testing.T) {
	cc := NewContainerContext("closed-scope")
	tok := Named[int]("n")
	if err := cc.Register(ProvideValue(tok, 1)); err != nil {
		t.Fatal(err)
	}
	if err := cc.Start(); err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	scope, err := cc.CreateScope()
	if err != nil {
		t.Fatal(err)
	}
	ctx := scope.Context(context.Background())
	if err := scope.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve[int](ctx, tok); err == nil {
		t.Fatal("a scope closed before use resolved a dependency")
	}
	if n := len(cc.root.children); n != 0 {
		t.Fatalf("the root holds %d children, want 0", n)
	}
}

func TestDetachedScopeResolvesConcurrentlyIntoOneContainer(t *testing.T) {
	cc := NewContainerContext("concurrent-scope")
	if err := cc.Start(); err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	scope, err := cc.CreateScope()
	if err != nil {
		t.Fatal(err)
	}
	defer scope.Close()
	ctx := scope.Context(context.Background())

	const workers = 8
	got := make(chan *Container, workers)
	for range workers {
		go func() { got <- ScopeFrom(ctx) }()
	}
	first := <-got
	for range workers - 1 {
		if c := <-got; c != first {
			t.Fatal("concurrent first resolutions created two containers")
		}
	}
}

func TestDetachedScopeFirstUsedAfterTheRootClosedRefusesResolution(t *testing.T) {
	spectest.Proves(t, "go/dependency-injection", "cleanup", "a-child-created-under-a-closed-parent-is-closed")
	cc := NewContainerContext("closed-root")
	tok := Named[int]("n")
	if err := cc.Register(ProvideValue(tok, 1)); err != nil {
		t.Fatal(err)
	}
	if err := cc.Start(); err != nil {
		t.Fatal(err)
	}
	scope, err := cc.CreateScope()
	if err != nil {
		t.Fatal(err)
	}
	ctx := scope.Context(context.Background())
	if err := cc.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve[int](ctx, tok); err == nil {
		t.Fatal("a scope first used after its root closed resolved a dependency")
	}
	if n := len(cc.root.children); n != 0 {
		t.Fatalf("the closed root holds %d children, want 0", n)
	}
	if err := scope.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDetachedScopeClosedBeforeUseStillAnswersHas(t *testing.T) {
	cc := NewContainerContext("closed-has")
	tok := Named[int]("n")
	if err := cc.Register(ProvideValue(tok, 1)); err != nil {
		t.Fatal(err)
	}
	if err := cc.Start(); err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	scope, err := cc.CreateScope()
	if err != nil {
		t.Fatal(err)
	}
	if err := scope.Close(); err != nil {
		t.Fatal(err)
	}
	if !scope.Has(tok) {
		t.Fatal("Has = false on a scope closed before use, want the parent's answer")
	}
}
