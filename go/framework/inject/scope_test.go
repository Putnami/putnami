package inject

import (
	"context"
	"fmt"
	"testing"
)

func TestScopeFrom_NoScope(t *testing.T) {
	scope := ScopeFrom(context.Background())
	if scope != nil {
		t.Error("expected nil scope from background context")
	}
}

func TestScopeFrom_WithScope(t *testing.T) {
	container := NewContainer("test", nil)
	ctx := withScope(context.Background(), container)
	scope := ScopeFrom(ctx)
	if scope == nil {
		t.Fatal("expected non-nil scope")
	}
	if scope.name != "test" {
		t.Errorf("scope name = %q, want test", scope.name)
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
