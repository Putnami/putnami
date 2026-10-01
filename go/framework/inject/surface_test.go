package inject

import (
	"context"
	"strings"
	"testing"

	"go.putnami.dev/errors"
)

// TestContainerContext_List_TaggedResolution covers ContainerContext.List:
// every tagged provider is resolved exactly once, untagged providers are
// excluded, and no result is silently dropped.
func TestContainerContext_List_TaggedResolution(t *testing.T) {
	cc := NewContainerContext("app")
	mustRegister(t, cc, Provide(Named[string]("x"), func(_ Resolver) (any, error) { return "x", nil }, WithTags("items")))
	mustRegister(t, cc, Provide(Named[string]("y"), func(_ Resolver) (any, error) { return "y", nil }, WithTags("items")))
	mustRegister(t, cc, Provide(Named[string]("z"), func(_ Resolver) (any, error) { return "z", nil }))
	if err := cc.Start(); err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	results, err := cc.List(Tagged[string]("items"))
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 tagged results, got %d: %v", len(results), results)
	}
	got := make(map[string]bool, len(results))
	for _, r := range results {
		got[r.(string)] = true
	}
	if !got["x"] || !got["y"] {
		t.Errorf("expected results x and y, got %v", results)
	}
}

func TestContainerContext_List_NotStarted(t *testing.T) {
	cc := NewContainerContext("app")
	if _, err := cc.List(Tagged[string]("items")); !errors.Is(err, CodeIllegalState) {
		t.Errorf("expected CodeIllegalState before Start, got %v", err)
	}
}

func TestContainerContext_Has(t *testing.T) {
	cc := NewContainerContext("app")
	tok := Named[int]("n")
	mustRegister(t, cc, ProvideValue(tok, 7))
	if err := cc.Start(); err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	if !cc.Has(tok) {
		t.Error("Has should be true for a registered token")
	}
	if cc.Has(Named[int]("missing")) {
		t.Error("Has should be false for an unregistered token")
	}
}

// TestScopeAdapter_HasAndList covers the ScopeContext adapter returned by
// CreateScope (scopedContainerAdapter.Has / .List).
func TestScopeAdapter_HasAndList(t *testing.T) {
	cc := NewContainerContext("app")
	mustRegister(t, cc, Provide(Named[string]("a"), func(_ Resolver) (any, error) { return "a", nil },
		WithTags("g"), WithScope(Scoped), WithLazy()))
	tok := Named[int]("n")
	mustRegister(t, cc, ProvideValue(tok, 1))
	if err := cc.Start(); err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	scope, err := cc.CreateScope()
	if err != nil {
		t.Fatal(err)
	}
	defer scope.Close()

	if !scope.Has(tok) {
		t.Error("scope.Has should see providers inherited from the root")
	}
	list, err := scope.List(Tagged[string]("g"))
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].(string) != "a" {
		t.Errorf("scope.List returned %v, want [a]", list)
	}
}

func TestIsScopedProvider(t *testing.T) {
	cc := NewContainerContext("app")
	scopedTok := Named[string]("scoped")
	singletonTok := Named[string]("singleton")
	mustRegister(t, cc, Provide(scopedTok, func(_ Resolver) (any, error) { return "s", nil }, WithScope(Scoped), WithLazy()))
	mustRegister(t, cc, ProvideValue(singletonTok, "v"))
	if err := cc.Start(); err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	if !cc.IsScopedProvider(scopedTok) {
		t.Error("scoped provider should report scoped")
	}
	if cc.IsScopedProvider(singletonTok) {
		t.Error("singleton provider must not report scoped")
	}
	if cc.IsScopedProvider(Named[string]("missing")) {
		t.Error("unregistered token must not report scoped")
	}
}

// TestFork_OverrideFactory covers ContainerContextFork.Override (the
// factory-based override; OverrideValue is covered elsewhere).
func TestFork_OverrideFactory(t *testing.T) {
	cc := NewContainerContext("app")
	tok := TokenOf[*testDB]()
	mustRegister(t, cc, Provide(tok, func(_ Resolver) (any, error) { return &testDB{DSN: "real"}, nil }))
	if err := cc.Start(); err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	forked, err := cc.Fork().
		Override(tok, func(_ Resolver) (any, error) { return &testDB{DSN: "mock"}, nil }).
		Start()
	if err != nil {
		t.Fatal(err)
	}
	defer forked.Close()

	val, err := forked.Get(tok)
	if err != nil {
		t.Fatal(err)
	}
	if got := val.(*testDB).DSN; got != "mock" {
		t.Errorf("expected overridden DSN 'mock', got %q", got)
	}
}

// TestScope_ResolveAllTyped covers the package-level ResolveAll[T] helper.
func TestScope_ResolveAllTyped(t *testing.T) {
	cc := NewContainerContext("app")
	mustRegister(t, cc, Provide(Named[string]("a"), func(_ Resolver) (any, error) { return "a", nil }, WithTags("g")))
	mustRegister(t, cc, Provide(Named[string]("b"), func(_ Resolver) (any, error) { return "b", nil }, WithTags("g")))
	if err := cc.Start(); err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	err := cc.Scope(context.Background(), func(ctx context.Context, _ ScopeContext) error {
		vals, err := ResolveAll[string](ctx, Tagged[string]("g"))
		if err != nil {
			return err
		}
		if len(vals) != 2 {
			t.Errorf("ResolveAll returned %d values, want 2", len(vals))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestResolver_ResolveAllInFactory covers Resolver.ResolveAll, the interface
// method a factory uses to collect tagged dependencies during construction.
func TestResolver_ResolveAllInFactory(t *testing.T) {
	c := NewContainer("test", nil)
	mustRegister(t, c, Provide(Named[string]("p1"), func(_ Resolver) (any, error) { return "p1", nil }, WithTags("plugin")))
	mustRegister(t, c, Provide(Named[string]("p2"), func(_ Resolver) (any, error) { return "p2", nil }, WithTags("plugin")))

	type registry struct{ plugins []string }
	regToken := TokenOf[*registry]()
	mustRegister(t, c, Provide(regToken, func(r Resolver) (any, error) {
		vals, err := r.ResolveAll(Tagged[string]("plugin"))
		if err != nil {
			return nil, err
		}
		reg := &registry{}
		for _, v := range vals {
			reg.plugins = append(reg.plugins, v.(string))
		}
		return reg, nil
	}))

	val, err := c.Get(regToken)
	if err != nil {
		t.Fatal(err)
	}
	if reg := val.(*registry); len(reg.plugins) != 2 {
		t.Errorf("expected 2 plugins collected via Resolver.ResolveAll, got %d: %v", len(reg.plugins), reg.plugins)
	}
}

func TestResolveAll_NoScope(t *testing.T) {
	_, err := ResolveAll[string](context.Background(), Tagged[string]("g"))
	if err == nil {
		t.Fatal("expected an error when no scope is active in the context")
	}
	if !errors.Is(err, CodeNotRegistered) {
		t.Errorf("expected CodeNotRegistered, got %v", err)
	}
}

func TestFormatResolutionChain(t *testing.T) {
	chain := []Token{Named[string]("a"), TokenOf[int]()}
	want := chain[0].Name() + " -> " + chain[1].Name()
	if got := formatResolutionChain(chain); got != want {
		t.Errorf("formatResolutionChain = %q, want %q", got, want)
	}
	if got := formatResolutionChain(nil); got != "" {
		t.Errorf("empty chain should format to %q, got %q", "", got)
	}
}

func TestTokenPredicatesAndTagName(t *testing.T) {
	named := Named[string]("x")
	class := TokenOf[string]()
	tag := Tagged[string]("t")

	if !isNamedToken(named) {
		t.Error("Named token should satisfy isNamedToken")
	}
	if isNamedToken(class) {
		t.Error("class token is not a named token")
	}
	if !isClassToken(class) {
		t.Error("class token should satisfy isClassToken")
	}
	if isClassToken(named) {
		t.Error("named token is not a class token")
	}

	if name := tag.Name(); !strings.Contains(name, "t") {
		t.Errorf("tag selector name %q should mention the tag", name)
	}
}
