package inject

import (
	"context"
	"errors"
	"testing"
)

// recordingFinalizer implements ScopeFinalizer and records how it was finalized.
type recordingFinalizer struct {
	calls      int
	gotOutcome error
	hadOutcome bool
	retErr     error
}

func (f *recordingFinalizer) FinalizeScope(_ context.Context, outcome error) error {
	f.calls++
	f.gotOutcome = outcome
	f.hadOutcome = true
	return f.retErr
}

// nonFinalizer is a scoped value that does NOT implement ScopeFinalizer, so it
// must be ignored by Finalize.
type nonFinalizer struct{}

// TestScope_Finalize_RunsFinalizerWithOutcome verifies Finalize invokes a
// materialized scoped ScopeFinalizer exactly once with the given outcome.
func TestScope_Finalize_RunsFinalizerWithOutcome(t *testing.T) {
	t.Run("success outcome", func(t *testing.T) {
		cc := NewContainerContext("t")
		token := TokenOf[*recordingFinalizer]()
		if err := cc.Register(Provide(token, func(_ Resolver) (any, error) {
			return &recordingFinalizer{}, nil
		}, WithScope(Scoped))); err != nil {
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
		v, err := scope.Get(token)
		if err != nil {
			t.Fatal(err)
		}
		f := v.(*recordingFinalizer)

		if err := scope.Finalize(context.Background(), nil); err != nil {
			t.Fatalf("finalize: %v", err)
		}
		if f.calls != 1 {
			t.Errorf("FinalizeScope calls = %d, want 1", f.calls)
		}
		if !f.hadOutcome || f.gotOutcome != nil {
			t.Errorf("expected a nil (success) outcome, got %v (had=%v)", f.gotOutcome, f.hadOutcome)
		}
	})

	t.Run("failure outcome", func(t *testing.T) {
		cc := NewContainerContext("t")
		token := TokenOf[*recordingFinalizer]()
		if err := cc.Register(Provide(token, func(_ Resolver) (any, error) {
			return &recordingFinalizer{}, nil
		}, WithScope(Scoped))); err != nil {
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
		v, err := scope.Get(token)
		if err != nil {
			t.Fatal(err)
		}
		f := v.(*recordingFinalizer)

		boom := errors.New("handler failed")
		if err := scope.Finalize(context.Background(), boom); err != nil {
			t.Fatalf("finalize: %v", err)
		}
		if f.gotOutcome != boom {
			t.Errorf("outcome = %v, want %v", f.gotOutcome, boom)
		}
	})
}

// TestScope_Finalize_NoFinalizer_IsNoop verifies a scope whose materialized
// values do not implement ScopeFinalizer finalizes cleanly (no error, nothing
// to do).
func TestScope_Finalize_NoFinalizer_IsNoop(t *testing.T) {
	cc := NewContainerContext("t")
	token := TokenOf[*nonFinalizer]()
	if err := cc.Register(Provide(token, func(_ Resolver) (any, error) {
		return &nonFinalizer{}, nil
	}, WithScope(Scoped))); err != nil {
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
	if _, err := scope.Get(token); err != nil {
		t.Fatal(err)
	}
	if err := scope.Finalize(context.Background(), nil); err != nil {
		t.Errorf("finalize with no ScopeFinalizer should be a no-op, got %v", err)
	}
}

// TestScope_Finalize_IgnoresParentSingletons pins the lifetime invariant: only
// values materialized IN the scope are finalized. A singleton (cached in the
// root container, app-lifetime) must never be committed/rolled back per scope.
func TestScope_Finalize_IgnoresParentSingletons(t *testing.T) {
	cc := NewContainerContext("t")
	singletonToken := Named[*recordingFinalizer]("singleton")
	scopedToken := Named[*recordingFinalizer]("scoped")
	singleton := &recordingFinalizer{}
	scoped := &recordingFinalizer{}

	if err := cc.Register(Provide(singletonToken, func(_ Resolver) (any, error) {
		return singleton, nil
	})); err != nil { // Singleton (default)
		t.Fatal(err)
	}
	if err := cc.Register(Provide(scopedToken, func(_ Resolver) (any, error) {
		return scoped, nil
	}, WithScope(Scoped))); err != nil {
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
	// Materialize both through the scope: the singleton caches in root, the
	// scoped instance caches in the scope container.
	if _, err := scope.Get(singletonToken); err != nil {
		t.Fatal(err)
	}
	if _, err := scope.Get(scopedToken); err != nil {
		t.Fatal(err)
	}

	if err := scope.Finalize(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if scoped.calls != 1 {
		t.Errorf("scoped finalizer calls = %d, want 1", scoped.calls)
	}
	if singleton.calls != 0 {
		t.Errorf("singleton finalizer calls = %d, want 0 (never finalized per scope)", singleton.calls)
	}
}

// TestScope_Finalize_AggregatesErrors verifies Finalize aggregates errors from
// multiple finalizers rather than dropping any.
func TestScope_Finalize_AggregatesErrors(t *testing.T) {
	cc := NewContainerContext("t")
	tokenA := Named[*recordingFinalizer]("a")
	tokenB := Named[*recordingFinalizer]("b")
	if err := cc.Register(Provide(tokenA, func(_ Resolver) (any, error) {
		return &recordingFinalizer{retErr: errors.New("A failed")}, nil
	}, WithScope(Scoped))); err != nil {
		t.Fatal(err)
	}
	if err := cc.Register(Provide(tokenB, func(_ Resolver) (any, error) {
		return &recordingFinalizer{retErr: errors.New("B failed")}, nil
	}, WithScope(Scoped))); err != nil {
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
	if _, err := scope.Get(tokenA); err != nil {
		t.Fatal(err)
	}
	if _, err := scope.Get(tokenB); err != nil {
		t.Fatal(err)
	}

	err = scope.Finalize(context.Background(), nil)
	if err == nil {
		t.Fatal("expected an aggregated error from failing finalizers")
	}
}
