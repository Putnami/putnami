package inject

import (
	"context"
	"fmt"
	"testing"

	"go.putnami.dev/errors"

	"go.putnami.dev/protocol/features/spectest"
)

// --- Token Tests ---

func TestTokenOf(t *testing.T) {
	token1 := TokenOf[string]()
	token2 := TokenOf[string]()
	token3 := TokenOf[int]()

	if token1.Key() != token2.Key() {
		t.Errorf("same type tokens should have same key: %s != %s", token1.Key(), token2.Key())
	}
	if token1.Key() == token3.Key() {
		t.Error("different type tokens should have different keys")
	}
}

func TestNamed(t *testing.T) {
	token1 := Named[string]("primary")
	token2 := Named[string]("primary")
	token3 := Named[string]("secondary")
	token4 := Named[int]("primary")

	if token1.Key() != token2.Key() {
		t.Error("same name+type should have same key")
	}
	if token1.Key() == token3.Key() {
		t.Error("different names should have different keys")
	}
	if token1.Key() == token4.Key() {
		t.Error("different types should have different keys")
	}
}

func TestTagged(t *testing.T) {
	tag := Tagged[string]("service")
	if tag.Tag() != "service" {
		t.Errorf("expected tag 'service', got %q", tag.Tag())
	}
	if !isTagSelector(tag) {
		t.Error("should be a tag selector")
	}
}

func TestTokenName(t *testing.T) {
	if name := TokenName(nil); name != "<nil>" {
		t.Errorf("nil token name should be <nil>, got %q", name)
	}
	if name := TokenName(Named[string]("db")); name == "" {
		t.Error("token name should not be empty")
	}
}

// --- Provider Tests ---

func TestProvide(t *testing.T) {
	token := Named[string]("greeting")
	reg := Provide(token, func(_ Resolver) (any, error) {
		return "hello", nil
	})

	if reg.provider.Token.Key() != token.Key() {
		t.Error("provider token should match")
	}
	if reg.provider.Scope != Singleton {
		t.Errorf("default scope should be Singleton, got %s", reg.provider.Scope)
	}
	if reg.provider.Visibility != Public {
		t.Errorf("default visibility should be Public, got %s", reg.provider.Visibility)
	}
}

func TestProvideWithOptions(t *testing.T) {
	token := Named[string]("scoped")
	reg := Provide(token,
		func(_ Resolver) (any, error) { return "val", nil },
		WithScope(Scoped),
		WithVisibility(Private),
		WithTags("tag1", "tag2"),
		WithLazy(),
	)

	p := reg.provider
	if p.Scope != Scoped {
		t.Errorf("expected Scoped, got %s", p.Scope)
	}
	if p.Visibility != Private {
		t.Errorf("expected Private, got %s", p.Visibility)
	}
	if len(p.Tags) != 2 || p.Tags[0] != "tag1" {
		t.Errorf("expected tags [tag1, tag2], got %v", p.Tags)
	}
	if !p.Lazy {
		t.Error("expected Lazy to be true")
	}
}

func TestProvideValue(t *testing.T) {
	token := Named[int]("port")
	reg := ProvideValue(token, 8080)

	c := NewContainer("test", nil)
	if err := c.Register(reg); err != nil {
		t.Fatal(err)
	}

	val, err := c.Get(token)
	if err != nil {
		t.Fatal(err)
	}
	if val.(int) != 8080 {
		t.Errorf("expected 8080, got %v", val)
	}
}

// --- Container Tests ---

func TestContainerBasicResolution(t *testing.T) {
	spectest.Proves(t, "go/dependency-injection", "resolution", "token-based-resolution")
	c := NewContainer("test", nil)

	token := Named[string]("msg")
	c.Register(ProvideValue(token, "hello"))

	val, err := c.Get(token)
	if err != nil {
		t.Fatal(err)
	}
	if val.(string) != "hello" {
		t.Errorf("expected 'hello', got %v", val)
	}
}

func TestContainerSingletonCaching(t *testing.T) {
	c := NewContainer("test", nil)
	callCount := 0

	token := Named[int]("counter")
	c.Register(Provide(token, func(_ Resolver) (any, error) {
		callCount++
		return callCount, nil
	}))

	val1, _ := c.Get(token)
	val2, _ := c.Get(token)

	if val1 != val2 {
		t.Errorf("singleton should return same instance: %v != %v", val1, val2)
	}
	if callCount != 1 {
		t.Errorf("factory should be called once, called %d times", callCount)
	}
}

func TestContainerDependencyResolution(t *testing.T) {
	c := NewContainer("test", nil)

	nameToken := Named[string]("name")
	greetToken := Named[string]("greeting")

	c.Register(ProvideValue(nameToken, "World"))
	c.Register(Provide(greetToken,
		func(r Resolver) (any, error) {
			name, err := r.Resolve(nameToken)
			if err != nil {
				return nil, err
			}
			return fmt.Sprintf("Hello, %s!", name.(string)), nil
		},
		WithDeps(nameToken),
	))

	val, err := c.Get(greetToken)
	if err != nil {
		t.Fatal(err)
	}
	if val.(string) != "Hello, World!" {
		t.Errorf("expected 'Hello, World!', got %v", val)
	}
}

func TestContainerParentResolution(t *testing.T) {
	parent := NewContainer("parent", nil)
	child := parent.CreateChild("child")

	token := Named[string]("shared")
	parent.Register(ProvideValue(token, "from-parent"))

	val, err := child.Get(token)
	if err != nil {
		t.Fatal(err)
	}
	if val.(string) != "from-parent" {
		t.Errorf("expected 'from-parent', got %v", val)
	}
}

func TestContainerPrivateVisibility(t *testing.T) {
	spectest.Proves(t, "go/dependency-injection", "resolution", "private-visibility-enforced")
	parent := NewContainer("parent", nil)
	child := parent.CreateChild("child")

	token := Named[string]("secret")
	parent.Register(Provide(token,
		func(_ Resolver) (any, error) { return "secret-value", nil },
		WithVisibility(Private),
	))

	// Parent can access it
	if !parent.Has(token) {
		t.Error("parent should have the token")
	}

	// Child should NOT see private providers
	if child.Has(token) {
		t.Error("child should not see private providers")
	}

	_, err := child.Get(token)
	if err == nil {
		t.Error("child should not resolve private providers")
	}
}

func TestContainerTaggedResolution(t *testing.T) {
	spectest.Proves(t, "go/dependency-injection", "resolution", "tagged-resolution")
	c := NewContainer("test", nil)

	t1 := Named[string]("svc1")
	t2 := Named[string]("svc2")
	t3 := Named[string]("other")

	c.Register(Provide(t1, func(_ Resolver) (any, error) { return "service-1", nil }, WithTags("service")))
	c.Register(Provide(t2, func(_ Resolver) (any, error) { return "service-2", nil }, WithTags("service")))
	c.Register(Provide(t3, func(_ Resolver) (any, error) { return "other", nil }, WithTags("other")))

	filter := Tagged[string]("service")
	results, err := c.List(filter)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Errorf("expected 2 results, got %d", len(results))
	}
}

func TestContainerCircularDependency(t *testing.T) {
	spectest.Proves(t, "go/dependency-injection", "resolution", "cycles-rejected")
	c := NewContainer("test", nil)

	tokenA := Named[string]("a")
	tokenB := Named[string]("b")

	c.Register(Provide(tokenA,
		func(r Resolver) (any, error) {
			val, err := r.Resolve(tokenB)
			if err != nil {
				return nil, err
			}
			return "a+" + val.(string), nil
		},
		WithDeps(tokenB),
	))
	c.Register(Provide(tokenB,
		func(r Resolver) (any, error) {
			val, err := r.Resolve(tokenA)
			if err != nil {
				return nil, err
			}
			return "b+" + val.(string), nil
		},
		WithDeps(tokenA),
	))

	issues := c.Validate()
	hasCycle := false
	for _, issue := range issues {
		if errors.Is(issue, CodeCircularDep) {
			hasCycle = true
		}
	}
	if !hasCycle {
		t.Error("expected circular dependency to be detected")
	}
}

func TestContainerDuplicateProvider(t *testing.T) {
	spectest.Proves(t, "go/dependency-injection", "resolution", "duplicate-provider-rejected")
	c := NewContainer("test", nil)

	token := Named[string]("dup")
	c.Register(ProvideValue(token, "first"))

	err := c.Register(ProvideValue(token, "second"))
	if err == nil {
		t.Error("expected duplicate provider error")
	}

	if !errors.Is(err, CodeDuplicateProvider) {
		t.Errorf("expected CodeDuplicateProvider error, got %v", err)
	}
}

func TestContainerMissingDependency(t *testing.T) {
	spectest.Proves(t, "go/dependency-injection", "resolution", "token-missing-dependency-rejected")
	c := NewContainer("test", nil)

	token := Named[string]("missing")
	_, err := c.Get(token)

	if !errors.Is(err, CodeNotRegistered) {
		t.Errorf("expected CodeNotRegistered error, got %v", err)
	}
}

func TestContainerClose(t *testing.T) {
	spectest.Proves(t, "go/dependency-injection", "cleanup", "close-hook-runs-for-built-instance")
	c := NewContainer("test", nil)
	closed := false

	token := Named[string]("closeable")
	c.Register(Provide(token,
		func(_ Resolver) (any, error) { return "value", nil },
		WithOnClose(func() error { closed = true; return nil }),
	))

	// Resolve to trigger factory
	c.Get(token)

	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	if !closed {
		t.Error("close hook should have been called")
	}

	// Accessing after close should fail
	_, err := c.Get(token)
	if !errors.Is(err, CodeContainerClosed) {
		t.Errorf("expected CodeContainerClosed error, got %v", err)
	}
}

func TestContainerScopeViolation(t *testing.T) {
	c := NewContainer("test", nil)

	scopedToken := Named[string]("scoped")
	singletonToken := Named[string]("singleton")

	c.Register(Provide(scopedToken,
		func(_ Resolver) (any, error) { return "scoped-val", nil },
		WithScope(Scoped),
	))
	c.Register(Provide(singletonToken,
		func(_ Resolver) (any, error) { return "singleton-val", nil },
		WithScope(Singleton),
		WithDeps(scopedToken),
	))

	issues := c.Validate()
	hasScopeViolation := false
	for _, issue := range issues {
		if errors.Is(issue, CodeScopeViolation) {
			hasScopeViolation = true
		}
	}
	if !hasScopeViolation {
		t.Error("expected scope violation to be detected")
	}
}

func TestNewValidationErrorPreservesIssueCodes(t *testing.T) {
	// A multi-issue validation error must keep every contained code recoverable
	// via errors.Is, not just the aggregate CodeValidation.
	issues := []error{
		newCircularDependencyError([]Token{Named[string]("a"), Named[string]("b"), Named[string]("a")}),
		newNotRegisteredError(Named[string]("missing"), "required by a"),
	}
	err := newValidationError(issues)

	if !errors.Is(err, CodeValidation) {
		t.Errorf("expected CodeValidation, got %v", err)
	}
	if !errors.Is(err, CodeCircularDep) {
		t.Errorf("expected CodeCircularDep to be recoverable, got %v", err)
	}
	if !errors.Is(err, CodeNotRegistered) {
		t.Errorf("expected CodeNotRegistered to be recoverable, got %v", err)
	}
}

// --- ContainerContext Tests ---

// TestContainerContextStartAggregatesIssueCodes verifies the documented contract
// that errors.Is on the error returned from Start() matches every underlying
// validation code even when several issues fail at once.
func TestContainerContextStartAggregatesIssueCodes(t *testing.T) {
	spectest.Proves(t, "go/dependency-injection", "startup", "start-validates-graph")
	cc := NewContainerContext("app")

	tokenA := Named[string]("a")
	tokenB := Named[string]("b")
	tokenMissing := Named[string]("missing")

	// A depends on B (cycle with B) and on an unregistered token (missing dep).
	cc.Register(Provide(tokenA,
		func(r Resolver) (any, error) { return r.Resolve(tokenB) },
		WithDeps(tokenB, tokenMissing),
	))
	// B depends on A, closing the cycle.
	cc.Register(Provide(tokenB,
		func(r Resolver) (any, error) { return r.Resolve(tokenA) },
		WithDeps(tokenA),
	))

	err := cc.Start()
	if err == nil {
		t.Fatal("expected Start() to fail with validation issues")
	}
	if !errors.Is(err, CodeValidation) {
		t.Errorf("expected CodeValidation, got %v", err)
	}
	if !errors.Is(err, CodeCircularDep) {
		t.Errorf("expected CodeCircularDep to be recoverable from Start() error, got %v", err)
	}
	if !errors.Is(err, CodeNotRegistered) {
		t.Errorf("expected CodeNotRegistered to be recoverable from Start() error, got %v", err)
	}
}

func TestContainerContextLifecycle(t *testing.T) {
	cc := NewContainerContext("app")

	token := Named[string]("hello")
	cc.Register(ProvideValue(token, "world"))

	if err := cc.Start(); err != nil {
		t.Fatal(err)
	}

	val, err := cc.Get(token)
	if err != nil {
		t.Fatal(err)
	}
	if val.(string) != "world" {
		t.Errorf("expected 'world', got %v", val)
	}

	if err := cc.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestContainerContextScope(t *testing.T) {
	cc := NewContainerContext("app")

	token := Named[int]("request-id")
	cc.Register(Provide(token,
		func(_ Resolver) (any, error) { return 42, nil },
		WithScope(Scoped),
		WithLazy(),
	))

	if err := cc.Start(); err != nil {
		t.Fatal(err)
	}

	err := cc.Scope(context.Background(), func(ctx context.Context, scope ScopeContext) error {
		// Resolve from scope context
		val, err := scope.Get(token)
		if err != nil {
			return err
		}
		if val.(int) != 42 {
			return fmt.Errorf("expected 42, got %v", val)
		}

		// Also resolvable via Resolve helper with context
		intVal, err := Resolve[int](ctx, token)
		if err != nil {
			return err
		}
		if intVal != 42 {
			return fmt.Errorf("expected 42 via Resolve, got %d", intVal)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	cc.Close()
}

func TestContainerContextFork(t *testing.T) {
	cc := NewContainerContext("app")

	token := Named[string]("greeting")
	cc.Register(ProvideValue(token, "hello"))
	cc.Start()

	// Fork and override
	forked, err := cc.Fork().
		OverrideValue(token, "mocked").
		Start()
	if err != nil {
		t.Fatal(err)
	}
	defer forked.Close()

	val, _ := forked.Get(token)
	if val.(string) != "mocked" {
		t.Errorf("expected 'mocked', got %v", val)
	}

	// Original should be unchanged
	origVal, _ := cc.Get(token)
	if origVal.(string) != "hello" {
		t.Errorf("original should be unchanged, got %v", origVal)
	}

	cc.Close()
}

func TestContainerContextRequirement(t *testing.T) {
	cc := NewContainerContext("app")
	cc.Require(Named[string]("required-token"))

	err := cc.Start()
	if err == nil {
		t.Error("expected requirement error")
	}

	if !errors.Is(err, CodeRequirementNotMet) {
		t.Errorf("expected CodeRequirementNotMet error, got %v", err)
	}
}

func TestContainerContextDetachedScope(t *testing.T) {
	cc := NewContainerContext("app")

	token := Named[string]("req-data")
	cc.Register(Provide(token,
		func(_ Resolver) (any, error) { return "per-request", nil },
		WithScope(Scoped),
		WithLazy(),
	))
	cc.Start()
	defer cc.Close()

	scope, err := cc.CreateScope()
	if err != nil {
		t.Fatal(err)
	}
	defer scope.Close()

	val, err := scope.Get(token)
	if err != nil {
		t.Fatal(err)
	}
	if val.(string) != "per-request" {
		t.Errorf("expected 'per-request', got %v", val)
	}
}

// --- ResolveAs Tests ---

func TestResolveAs(t *testing.T) {
	c := NewContainer("test", nil)

	token := Named[string]("val")
	c.Register(ProvideValue(token, "typed-value"))

	resolver := &containerResolver{container: c}
	val, err := ResolveAs[string](resolver, token)
	if err != nil {
		t.Fatal(err)
	}
	if val != "typed-value" {
		t.Errorf("expected 'typed-value', got %q", val)
	}
}

func TestResolveAsTypeMismatch(t *testing.T) {
	c := NewContainer("test", nil)

	token := Named[int]("val")
	c.Register(ProvideValue(token, "not-an-int"))

	resolver := &containerResolver{container: c}
	_, err := ResolveAs[int](resolver, token)
	if err == nil {
		t.Error("expected type mismatch error")
	}

	if !errors.Is(err, CodeTypeMismatch) {
		t.Errorf("expected CodeTypeMismatch error, got %v", err)
	}
}

// --- ProvideAlias Tests ---

// aliasReader and aliasWriter are interfaces both satisfied by *aliasService.
// They mirror the read/write split pattern from the issue.
type aliasReader interface {
	Read() string
}

type aliasWriter interface {
	Write(s string)
}

type aliasService struct{ v string }

func (s *aliasService) Read() string   { return s.v }
func (s *aliasService) Write(v string) { s.v = v }

// aliasOnlyReader is a value type that satisfies aliasReader but not aliasWriter.
type aliasOnlyReader struct{}

func (aliasOnlyReader) Read() string { return "static" }

func TestProvideAliasSameInstance(t *testing.T) {
	c := NewContainer("test", nil)

	svc := &aliasService{v: "initial"}
	c.Register(ProvideValue(TokenOf[aliasReader](), aliasReader(svc)))
	c.Register(ProvideAlias[aliasWriter, aliasReader]())

	readerVal, err := c.Get(TokenOf[aliasReader]())
	if err != nil {
		t.Fatal(err)
	}
	writerVal, err := c.Get(TokenOf[aliasWriter]())
	if err != nil {
		t.Fatal(err)
	}

	reader := readerVal.(aliasReader)
	writer := writerVal.(aliasWriter)

	// Both views must share the same underlying instance.
	writer.Write("updated")
	if got := reader.Read(); got != "updated" {
		t.Errorf("alias should share state with source: expected %q, got %q", "updated", got)
	}
}

func TestProvideAliasMissingSource(t *testing.T) {
	c := NewContainer("test", nil)
	c.Register(ProvideAlias[aliasWriter, aliasReader]())

	_, err := c.Get(TokenOf[aliasWriter]())
	if err == nil {
		t.Fatal("expected error when source is not registered")
	}
	if !errors.Is(err, CodeNotRegistered) {
		t.Errorf("expected CodeNotRegistered, got %v", err)
	}
}

func TestProvideAliasTypeMismatch(t *testing.T) {
	c := NewContainer("test", nil)

	// Source value satisfies aliasReader but not aliasWriter.
	c.Register(ProvideValue(TokenOf[aliasReader](), aliasReader(aliasOnlyReader{})))
	c.Register(ProvideAlias[aliasWriter, aliasReader]())

	_, err := c.Get(TokenOf[aliasWriter]())
	if err == nil {
		t.Fatal("expected type-mismatch error when source does not satisfy destination")
	}
	if !errors.Is(err, CodeTypeMismatch) {
		t.Errorf("expected CodeTypeMismatch, got %v", err)
	}
}

func TestProvideAliasDeclaresDep(t *testing.T) {
	reg := ProvideAlias[aliasWriter, aliasReader]()
	deps := reg.provider.Deps
	if len(deps) != 1 {
		t.Fatalf("alias should declare exactly one dependency, got %d", len(deps))
	}
	if deps[0].Key() != TokenOf[aliasReader]().Key() {
		t.Errorf("alias dep should be the source token: got %s", TokenName(deps[0]))
	}
}

func TestProvideAliasSourceFactoryRunsOnce(t *testing.T) {
	c := NewContainer("test", nil)

	calls := 0
	c.Register(Provide(TokenOf[aliasReader](), func(_ Resolver) (any, error) {
		calls++
		return aliasReader(&aliasService{v: "x"}), nil
	}))
	c.Register(ProvideAlias[aliasWriter, aliasReader]())

	for range 3 {
		if _, err := c.Get(TokenOf[aliasWriter]()); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Get(TokenOf[aliasReader]()); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Errorf("source factory should run once across alias and direct resolves, ran %d times", calls)
	}
}
