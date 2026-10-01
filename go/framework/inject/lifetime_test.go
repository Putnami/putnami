package inject

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.putnami.dev/errors"
	"go.putnami.dev/protocol/features/spectest"
)

type registrar interface {
	Register(Registration) error
}

func mustRegister(t *testing.T, r registrar, reg Registration) {
	t.Helper()
	if err := r.Register(reg); err != nil {
		t.Fatalf("register: %v", err)
	}
}

// TestSingleton_SharedAcrossScopes asserts the singleton lifetime contract:
// a singleton resolved through a scope is the same instance as when resolved
// from the root, and its factory — and its whole dependency subgraph — runs
// exactly once regardless of how many scopes resolve it.
func TestSingleton_SharedAcrossScopes(t *testing.T) {
	spectest.Proves(t, "go/dependency-injection", "lifetimes", "singleton-shared-across-scopes-and-root")
	var cfgCalls, dbCalls atomic.Int32

	cfgToken := Named[*testConfig]("cfg")
	dbToken := Named[*testDB]("db")

	cc := NewContainerContext("app")
	mustRegister(t, cc, Provide(cfgToken, func(_ Resolver) (any, error) {
		cfgCalls.Add(1)
		return &testConfig{Host: "h", Port: 1}, nil
	}, WithLazy()))
	mustRegister(t, cc, Provide(dbToken, func(r Resolver) (any, error) {
		dbCalls.Add(1)
		cfg, err := ResolveAs[*testConfig](r, cfgToken)
		if err != nil {
			return nil, err
		}
		return &testDB{DSN: cfg.Host}, nil
	}, WithDeps(cfgToken), WithLazy()))

	if err := cc.Start(); err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	// Resolve via two independent scopes.
	fromScopes := make([]*testDB, 0, 2)
	for range 2 {
		scope, err := cc.CreateScope()
		if err != nil {
			t.Fatal(err)
		}
		val, err := scope.Get(dbToken)
		if err != nil {
			t.Fatal(err)
		}
		fromScopes = append(fromScopes, val.(*testDB))
		if err := scope.Close(); err != nil {
			t.Fatal(err)
		}
	}

	// ...and from the root.
	rootVal, err := cc.Get(dbToken)
	if err != nil {
		t.Fatal(err)
	}
	rootDB := rootVal.(*testDB)

	if fromScopes[0] != fromScopes[1] || fromScopes[0] != rootDB {
		t.Errorf("singleton must be identity-equal across scopes and root: %p, %p, %p",
			fromScopes[0], fromScopes[1], rootDB)
	}
	if got := dbCalls.Load(); got != 1 {
		t.Errorf("singleton db factory ran %d times, want 1", got)
	}
	if got := cfgCalls.Load(); got != 1 {
		t.Errorf("singleton subgraph (config) ran %d times, want 1", got)
	}
}

// TestScoped_OncePerScope asserts the scoped lifetime contract: a scoped
// provider resolved twice within one scope returns the same instance, and a
// different scope gets a fresh instance.
func TestScoped_OncePerScope(t *testing.T) {
	spectest.Proves(t, "go/dependency-injection", "lifetimes", "scoped-instance-is-per-scope")
	var calls atomic.Int32
	tok := Named[*testUserRepository]("repo")

	cc := NewContainerContext("app")
	mustRegister(t, cc, Provide(tok, func(_ Resolver) (any, error) {
		calls.Add(1)
		return &testUserRepository{}, nil
	}, WithScope(Scoped), WithLazy()))
	if err := cc.Start(); err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	scope1, err := cc.CreateScope()
	if err != nil {
		t.Fatal(err)
	}
	defer scope1.Close()

	a, err := scope1.Get(tok)
	if err != nil {
		t.Fatal(err)
	}
	b, err := scope1.Get(tok)
	if err != nil {
		t.Fatal(err)
	}
	if a.(*testUserRepository) != b.(*testUserRepository) {
		t.Error("scoped provider must be identity-equal within one scope")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("scoped factory ran %d times in one scope, want 1", got)
	}

	scope2, err := cc.CreateScope()
	if err != nil {
		t.Fatal(err)
	}
	defer scope2.Close()

	c, err := scope2.Get(tok)
	if err != nil {
		t.Fatal(err)
	}
	if c.(*testUserRepository) == a.(*testUserRepository) {
		t.Error("scoped provider must be a fresh instance in a different scope")
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("scoped factory ran %d times across two scopes, want 2", got)
	}
}

// TestScoped_OnCloseRunsPerScope asserts that a scoped provider's OnClose hook
// runs when the scope that created the instance is closed — and is not
// (spuriously) run by the root container, which never instantiated it. Before
// the fix, scope-created instances were discarded without running OnClose.
func TestScoped_OnCloseRunsPerScope(t *testing.T) {
	spectest.Proves(t, "go/dependency-injection", "cleanup", "scoped-close-hooks-run-per-scope")
	var closes atomic.Int32
	tok := Named[*testDB]("scoped-db")

	cc := NewContainerContext("app")
	mustRegister(t, cc, Provide(tok, func(_ Resolver) (any, error) {
		return &testDB{DSN: "x"}, nil
	}, WithScope(Scoped), WithLazy(), WithOnClose(func() error {
		closes.Add(1)
		return nil
	})))
	if err := cc.Start(); err != nil {
		t.Fatal(err)
	}

	scope, err := cc.CreateScope()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scope.Get(tok); err != nil {
		t.Fatal(err)
	}
	if got := closes.Load(); got != 0 {
		t.Errorf("close hook ran %d times before scope close, want 0", got)
	}
	if err := scope.Close(); err != nil {
		t.Fatal(err)
	}
	if got := closes.Load(); got != 1 {
		t.Errorf("scoped close hook ran %d times after scope close, want 1", got)
	}

	// Closing the root context must not re-run the scoped hook.
	if err := cc.Close(); err != nil {
		t.Fatal(err)
	}
	if got := closes.Load(); got != 1 {
		t.Errorf("scoped close hook ran %d times after root close, want 1", got)
	}
}

// TestConcurrent_LazySingleton_NoSpuriousCycle asserts that concurrent first
// resolution of an uncached lazy singleton never produces a spurious
// circular-dependency error and that every caller observes the same instance.
// The slow factory widens the race window that the previous shared-state cycle
// detection tripped over.
func TestConcurrent_LazySingleton_NoSpuriousCycle(t *testing.T) {
	tok := Named[*testDB]("lazy")
	var calls atomic.Int32

	cc := NewContainerContext("app")
	mustRegister(t, cc, Provide(tok, func(_ Resolver) (any, error) {
		calls.Add(1)
		time.Sleep(20 * time.Millisecond)
		return &testDB{DSN: "lazy"}, nil
	}, WithLazy()))
	if err := cc.Start(); err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	const goroutines = 8
	var wg sync.WaitGroup
	wg.Add(goroutines)
	results := make([]*testDB, goroutines)
	errs := make([]error, goroutines)

	for i := range goroutines {
		go func() {
			defer wg.Done()
			val, err := cc.Get(tok)
			errs[i] = err
			if err == nil {
				results[i] = val.(*testDB)
			}
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: unexpected error: %v", i, err)
		}
		if errors.Is(err, CodeCircularDep) {
			t.Errorf("goroutine %d: spurious circular-dependency error", i)
		}
	}
	for i := 1; i < goroutines; i++ {
		if results[i] != results[0] {
			t.Errorf("goroutine %d observed a different singleton instance than goroutine 0", i)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("lazy singleton factory ran %d times under concurrent first resolution, want 1", got)
	}
}

// TestRuntimeCycle_ReportsFullPath asserts that a circular dependency which
// only manifests at runtime (hand-written factories without WithDeps, which
// Validate cannot see) surfaces an error whose message includes the full cycle
// path rather than just the re-entered token.
func TestRuntimeCycle_ReportsFullPath(t *testing.T) {
	spectest.Proves(t, "go/dependency-injection", "resolution", "runtime-cycle-reports-the-full-path")
	c := NewContainer("test", nil)

	a := Named[string]("a")
	b := Named[string]("b")

	mustRegister(t, c, Provide(a, func(r Resolver) (any, error) {
		return r.Resolve(b)
	}))
	mustRegister(t, c, Provide(b, func(r Resolver) (any, error) {
		return r.Resolve(a)
	}))

	_, err := c.Get(a)
	if err == nil {
		t.Fatal("expected a circular-dependency error")
	}
	if !errors.Is(err, CodeCircularDep) {
		t.Fatalf("expected CodeCircularDep, got %v", err)
	}

	wantPath := a.Name() + " -> " + b.Name() + " -> " + a.Name()
	if !strings.Contains(err.Error(), wantPath) {
		t.Errorf("error should report the full cycle path %q, got: %s", wantPath, err.Error())
	}
}

func TestConcurrentRuntimeCycle_DoesNotDeadlock(t *testing.T) {
	spectest.Proves(t, "go/dependency-injection", "resolution", "runtime-cycle-rejected-across-goroutines")
	// An undeclared cycle a→b→a, first-resolved concurrently on two goroutines,
	// must return a circular-dependency error rather than deadlocking (each
	// goroutine blocking on the other's in-flight build).
	c := NewContainer("test", nil)
	a := Named[string]("a")
	b := Named[string]("b")

	// Barrier: both builds must exist before either resolves the other, so the
	// cross-goroutine wait path is exercised deterministically.
	aStarted := make(chan struct{})
	bStarted := make(chan struct{})

	mustRegister(t, c, Provide(a, func(r Resolver) (any, error) {
		close(aStarted)
		<-bStarted
		return r.Resolve(b)
	}))
	mustRegister(t, c, Provide(b, func(r Resolver) (any, error) {
		close(bStarted)
		<-aStarted
		return r.Resolve(a)
	}))

	errs := make(chan error, 2)
	go func() { _, err := c.Get(a); errs <- err }()
	go func() { _, err := c.Get(b); errs <- err }()

	circular := 0
	for i := 0; i < 2; i++ {
		select {
		case err := <-errs:
			if err != nil && errors.Is(err, CodeCircularDep) {
				circular++
			}
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent runtime cycle deadlocked instead of returning an error")
		}
	}
	if circular == 0 {
		t.Error("expected at least one resolution to fail with CodeCircularDep")
	}
}

func TestConcurrentRuntimeCycle_ThreeNode_DoesNotDeadlock(t *testing.T) {
	spectest.Proves(t, "go/dependency-injection", "resolution", "runtime-cycle-rejected-through-a-nested-create")
	// An undeclared 3-node cycle a→b→c→a whose closing edge is traversed through
	// a nested create (goroutine building c creates a, which then collides on b)
	// must still be detected instead of deadlocking. A 2-node cycle exercises only
	// the collision-wait edges; this one only closes through the create-descent
	// edge, so it regresses if that edge is missing from the wait-for graph.
	c := NewContainer("test", nil)
	a := Named[string]("a")
	b := Named[string]("b")
	cc := Named[string]("c")

	// Barrier: both top-level builds (b and c) must exist before either factory
	// descends, so b→c and c→(create a)→b both hit the in-flight builds
	// deterministically and the cross-goroutine cycle is forced every run.
	bStarted := make(chan struct{})
	cStarted := make(chan struct{})

	mustRegister(t, c, Provide(a, func(r Resolver) (any, error) {
		return r.Resolve(b)
	}))
	mustRegister(t, c, Provide(b, func(r Resolver) (any, error) {
		close(bStarted)
		<-cStarted
		return r.Resolve(cc)
	}))
	mustRegister(t, c, Provide(cc, func(r Resolver) (any, error) {
		close(cStarted)
		<-bStarted
		return r.Resolve(a)
	}))

	errs := make(chan error, 2)
	go func() { _, err := c.Get(cc); errs <- err }()
	go func() { _, err := c.Get(b); errs <- err }()

	circular := 0
	for i := 0; i < 2; i++ {
		select {
		case err := <-errs:
			if err != nil && errors.Is(err, CodeCircularDep) {
				circular++
			}
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent 3-node runtime cycle deadlocked instead of returning an error")
		}
	}
	if circular == 0 {
		t.Error("expected at least one resolution to fail with CodeCircularDep")
	}
}

// TestSingletonResolvingUndeclaredScopedViolatesScope asserts that a singleton
// whose factory resolves a scoped provider it never declared via WithDeps (so
// Validate cannot see the edge) fails with CodeScopeViolation at resolution
// time, rather than silently caching the "scoped" instance at the singleton's
// (root) lifetime.
func TestSingletonResolvingUndeclaredScopedViolatesScope(t *testing.T) {
	spectest.Proves(t, "go/dependency-injection", "lifetimes", "singleton-depending-on-scoped-violates-scope")
	scopedTok := Named[*testUserRepository]("scoped-repo")
	singletonTok := Named[string]("svc")

	cc := NewContainerContext("app")
	mustRegister(t, cc, Provide(scopedTok, func(_ Resolver) (any, error) {
		return &testUserRepository{}, nil
	}, WithScope(Scoped), WithLazy()))
	// The singleton resolves the scoped provider without declaring it, so the
	// scope-violation validator (which only inspects declared deps) misses it.
	mustRegister(t, cc, Provide(singletonTok, func(r Resolver) (any, error) {
		if _, err := r.Resolve(scopedTok); err != nil {
			return nil, err
		}
		return "ok", nil
	}))

	err := cc.Start()
	if err == nil {
		t.Fatal("expected Start() to fail: singleton resolved an undeclared scoped provider")
	}
	if !errors.Is(err, CodeScopeViolation) {
		t.Errorf("expected CodeScopeViolation, got %v", err)
	}
}

// TestResolveScopedFromRootViolatesScope asserts that resolving a scoped
// provider directly from the root context (with no active scope to own the
// instance) is a scope violation rather than a silent root-lifetime cache.
func TestResolveScopedFromRootViolatesScope(t *testing.T) {
	spectest.Proves(t, "go/dependency-injection", "lifetimes", "root-resolution-of-scoped-provider-fails")
	scopedTok := Named[*testUserRepository]("scoped-only")

	cc := NewContainerContext("app")
	mustRegister(t, cc, Provide(scopedTok, func(_ Resolver) (any, error) {
		return &testUserRepository{}, nil
	}, WithScope(Scoped), WithLazy()))
	if err := cc.Start(); err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	_, err := cc.Get(scopedTok)
	if err == nil {
		t.Fatal("expected error resolving a scoped provider from the root context")
	}
	if !errors.Is(err, CodeScopeViolation) {
		t.Errorf("expected CodeScopeViolation, got %v", err)
	}
}

// TestStart_FailureClosesBuiltSingletonsAndResetsState asserts the eager-boot
// rollback contract: when a non-lazy singleton factory fails during Start, any
// singleton already constructed earlier in the same boot has its OnClose hook
// run (so its resources are released, not leaked), and the context is reset to
// a not-started state so Get reports "not started" and Start can be retried.
//
// The failing singleton declares the good one as a dependency, forcing the good
// one to be constructed first regardless of provider-map iteration order.
func TestStart_FailureClosesBuiltSingletonsAndResetsState(t *testing.T) {
	spectest.Proves(t, "go/dependency-injection", "startup", "start-failure-closes-built-singletons-and-resets-to-idle")
	var goodCloses atomic.Int32

	goodTok := Named[*testDB]("good")
	badTok := Named[string]("bad")

	cc := NewContainerContext("app")
	mustRegister(t, cc, Provide(goodTok, func(_ Resolver) (any, error) {
		return &testDB{DSN: "good"}, nil
	}, WithOnClose(func() error {
		goodCloses.Add(1)
		return nil
	})))
	// The bad singleton resolves the good one first (so it is already built),
	// then fails. Declaring the dep also makes the ordering deterministic.
	mustRegister(t, cc, Provide(badTok, func(r Resolver) (any, error) {
		if _, err := ResolveAs[*testDB](r, goodTok); err != nil {
			return nil, err
		}
		return nil, errors.New(errors.Code("boot.boom"), "factory exploded")
	}, WithDeps(goodTok)))

	err := cc.Start()
	if err == nil {
		t.Fatal("expected Start() to fail when a non-lazy singleton factory errors")
	}
	if !errors.Is(err, CodeFactoryFailed) {
		t.Errorf("expected CodeFactoryFailed from Start(), got %v", err)
	}

	// The already-built good singleton must have been disposed exactly once.
	if got := goodCloses.Load(); got != 1 {
		t.Errorf("already-built singleton OnClose ran %d times on failed Start, want 1", got)
	}

	// State must be reset: Get reports not-started rather than a resolution
	// error from a half-built container.
	_, getErr := cc.Get(goodTok)
	if getErr == nil {
		t.Fatal("expected Get to fail after a failed Start (context not started)")
	}
	if !errors.Is(getErr, CodeIllegalState) {
		t.Errorf("expected CodeIllegalState (not started) from Get after failed Start, got %v", getErr)
	}

	// Closing a never-successfully-started context is safe and does not re-run
	// the singleton hook (it already ran during rollback).
	if err := cc.Close(); err != nil {
		t.Fatalf("Close after failed Start: %v", err)
	}
	if got := goodCloses.Load(); got != 1 {
		t.Errorf("singleton OnClose ran %d times after failed Start + Close, want 1", got)
	}
}

// TestStart_RetryableAfterRequirementFailure asserts that a Start failing on an
// unmet requirement leaves the context Idle so a later Start (after the
// requirement is provided) succeeds, rather than wedging in CodeIllegalState.
func TestStart_RetryableAfterRequirementFailure(t *testing.T) {
	spectest.Proves(t, "go/dependency-injection", "startup", "start-retryable-after-failure")
	reqTok := Named[string]("late-config")

	cc := NewContainerContext("app")
	cc.Require(reqTok)

	if err := cc.Start(); err == nil {
		t.Fatal("expected first Start() to fail: requirement not met")
	} else if !errors.Is(err, CodeRequirementNotMet) {
		t.Errorf("expected CodeRequirementNotMet, got %v", err)
	}

	// Satisfy the requirement, then retry. A wedged context would reject this
	// with CodeIllegalState ("already started").
	mustRegister(t, cc, ProvideValue(reqTok, "ready"))
	if err := cc.Start(); err != nil {
		t.Fatalf("retry Start() after satisfying requirement should succeed, got %v", err)
	}
	defer cc.Close()

	val, err := cc.Get(reqTok)
	if err != nil {
		t.Fatalf("Get after successful retry: %v", err)
	}
	if val.(string) != "ready" {
		t.Errorf("expected resolved value 'ready', got %v", val)
	}
}

// TestStart_RetryRebuildsAndClosesAfterFactoryFailure asserts the full
// failure-then-recovery path: a Start that fails mid-resolution can be retried
// once the offending factory is fixed, the singleton subgraph rebuilds, and a
// subsequent Close runs the (registration-time) close hooks exactly once —
// proving rollback did not discard the static hook registrations.
func TestStart_RetryRebuildsAndClosesAfterFactoryFailure(t *testing.T) {
	var goodBuilds, goodCloses atomic.Int32
	var fail atomic.Bool
	fail.Store(true)

	goodTok := Named[*testDB]("good")
	gateTok := Named[string]("gate")

	cc := NewContainerContext("app")
	mustRegister(t, cc, Provide(goodTok, func(_ Resolver) (any, error) {
		goodBuilds.Add(1)
		return &testDB{DSN: "good"}, nil
	}, WithOnClose(func() error {
		goodCloses.Add(1)
		return nil
	})))
	mustRegister(t, cc, Provide(gateTok, func(r Resolver) (any, error) {
		if _, err := ResolveAs[*testDB](r, goodTok); err != nil {
			return nil, err
		}
		if fail.Load() {
			return nil, errors.New(errors.Code("boot.boom"), "gate closed")
		}
		return "open", nil
	}, WithDeps(goodTok)))

	if err := cc.Start(); err == nil {
		t.Fatal("expected first Start() to fail")
	}
	if got := goodBuilds.Load(); got != 1 {
		t.Errorf("good singleton built %d times on first (failed) Start, want 1", got)
	}
	if got := goodCloses.Load(); got != 1 {
		t.Errorf("good singleton closed %d times during rollback, want 1", got)
	}

	// Fix the factory and retry.
	fail.Store(false)
	if err := cc.Start(); err != nil {
		t.Fatalf("retry Start() should succeed after fixing the factory, got %v", err)
	}
	if got := goodBuilds.Load(); got != 2 {
		t.Errorf("good singleton should be rebuilt on retry (total 2 builds), got %d", got)
	}

	// Close must run the close hook exactly once for the now-live singleton,
	// confirming the registration-time hook survived the rollback.
	if err := cc.Close(); err != nil {
		t.Fatalf("Close after successful retry: %v", err)
	}
	if got := goodCloses.Load(); got != 2 {
		t.Errorf("good singleton close hook should run on Close after retry (total 2), got %d", got)
	}
}

// TestStartLazy_DoesNotRunNonLazySingletonFactory asserts StartLazy's core
// contract — the property the describe path (PUTNAMI_DESCRIBE) relies on to
// avoid reaching external systems at metadata-harvest time: no non-lazy
// singleton factory runs during StartLazy, yet the provider is still resolvable
// (built on demand) afterwards.
func TestStartLazy_DoesNotRunNonLazySingletonFactory(t *testing.T) {
	var builds atomic.Int32
	tok := Named[*testDB]("eager-by-default")

	cc := NewContainerContext("app")
	// Note: a plain singleton, NOT marked WithLazy — under Start() this would be
	// eagerly constructed. StartLazy must treat it as if it were lazy.
	mustRegister(t, cc, Provide(tok, func(_ Resolver) (any, error) {
		builds.Add(1)
		return &testDB{DSN: "external"}, nil
	}))

	if err := cc.StartLazy(); err != nil {
		t.Fatalf("StartLazy: %v", err)
	}
	defer cc.Close()

	if got := builds.Load(); got != 0 {
		t.Fatalf("StartLazy ran a non-lazy singleton factory %d times, want 0 (must not touch external systems)", got)
	}

	// The provider is still wired — a later Get builds it on demand.
	val, err := cc.Get(tok)
	if err != nil {
		t.Fatalf("Get after StartLazy: %v", err)
	}
	if val.(*testDB).DSN != "external" {
		t.Errorf("expected resolved DSN 'external', got %q", val.(*testDB).DSN)
	}
	if got := builds.Load(); got != 1 {
		t.Errorf("factory should run exactly once on first Get after StartLazy, ran %d times", got)
	}
}

// TestStartLazy_StillValidates asserts that StartLazy performs graph validation
// (it only skips eager *resolution*, not the validation pass). A missing
// dependency must surface at StartLazy time rather than being deferred to a
// later Get during describe.
func TestStartLazy_StillValidates(t *testing.T) {
	svcTok := Named[string]("svc")
	missingTok := Named[*testDB]("missing-dep")

	cc := NewContainerContext("app")
	mustRegister(t, cc, Provide(svcTok, func(_ Resolver) (any, error) {
		return "ok", nil
	}, WithDeps(missingTok)))

	err := cc.StartLazy()
	if err == nil {
		t.Fatal("expected StartLazy to fail validation for a missing dependency")
	}
	if !errors.Is(err, CodeValidation) {
		t.Errorf("expected CodeValidation, got %v", err)
	}
	if !errors.Is(err, CodeNotRegistered) {
		t.Errorf("expected CodeNotRegistered to be recoverable from StartLazy error, got %v", err)
	}
}

// TestStartLazy_EnablesGetAndList asserts that after StartLazy the context is in
// the started state: Get and List both work (resolving lazily), matching the
// behavior callers rely on in describe mode where they enumerate providers.
func TestStartLazy_EnablesGetAndList(t *testing.T) {
	var builds atomic.Int32
	tok := Named[*testDB]("tagged-db")

	cc := NewContainerContext("app")
	mustRegister(t, cc, Provide(tok, func(_ Resolver) (any, error) {
		builds.Add(1)
		return &testDB{DSN: "lazy"}, nil
	}, WithTags("infra")))

	if err := cc.StartLazy(); err != nil {
		t.Fatalf("StartLazy: %v", err)
	}
	defer cc.Close()

	if got := builds.Load(); got != 0 {
		t.Fatalf("StartLazy built %d instances, want 0", got)
	}

	results, err := cc.List(Tagged[*testDB]("infra"))
	if err != nil {
		t.Fatalf("List after StartLazy: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 tagged result, got %d", len(results))
	}
	if results[0].(*testDB).DSN != "lazy" {
		t.Errorf("unexpected resolved value: %#v", results[0])
	}
}

// TestStart_EagerlyResolvesNonLazySingletons asserts the eager half of Start's
// contract: a singleton that is NOT marked lazy is constructed by Start itself,
// so a boot failure (an unreachable database, a port already bound) surfaces at
// startup rather than on the first request that happens to need it.
func TestStart_EagerlyResolvesNonLazySingletons(t *testing.T) {
	spectest.Proves(t, "go/dependency-injection", "startup", "start-eagerly-resolves-non-lazy-singletons")
	var eagerBuilds, lazyBuilds atomic.Int32

	eagerTok := Named[*testDB]("eager")
	lazyTok := Named[*testDB]("lazy")

	cc := NewContainerContext("app")
	mustRegister(t, cc, Provide(eagerTok, func(_ Resolver) (any, error) {
		eagerBuilds.Add(1)
		return &testDB{DSN: "eager"}, nil
	}))
	mustRegister(t, cc, Provide(lazyTok, func(_ Resolver) (any, error) {
		lazyBuilds.Add(1)
		return &testDB{DSN: "lazy"}, nil
	}, WithLazy()))

	if err := cc.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer cc.Close()

	if got := eagerBuilds.Load(); got != 1 {
		t.Errorf("non-lazy singleton factory ran %d times during Start, want 1", got)
	}
	if got := lazyBuilds.Load(); got != 0 {
		t.Errorf("lazy singleton factory ran %d times during Start, want 0", got)
	}

	// The eager instance Start built is the one Get returns — it was cached,
	// not rebuilt.
	if _, err := cc.Get(eagerTok); err != nil {
		t.Fatalf("Get after Start: %v", err)
	}
	if got := eagerBuilds.Load(); got != 1 {
		t.Errorf("non-lazy singleton was rebuilt on Get: factory ran %d times, want 1", got)
	}
}

// TestClose_ChildrenBeforeParentInReverseRegistrationOrder asserts the two
// ordering guarantees of container teardown. A child's instances may hold
// references into the parent's (a request repository over the parent's pool),
// and a later registration may depend on an earlier one, so cleanup must unwind
// in the opposite direction from construction: children first, then this
// container's own hooks last-registered-first.
func TestClose_ChildrenBeforeParentInReverseRegistrationOrder(t *testing.T) {
	spectest.Proves(t, "go/dependency-injection", "cleanup", "close-runs-children-before-parent")
	spectest.Proves(t, "go/dependency-injection", "cleanup", "close-hooks-run-in-reverse-registration-order")

	var mu sync.Mutex
	var order []string
	record := func(name string) func() error {
		return func() error {
			mu.Lock()
			defer mu.Unlock()
			order = append(order, name)
			return nil
		}
	}

	parent := NewContainer("parent", nil)
	parentFirst := Named[string]("parent-first")
	parentSecond := Named[string]("parent-second")
	mustRegister(t, parent, Provide(parentFirst,
		func(_ Resolver) (any, error) { return "a", nil },
		WithOnClose(record("parent-first"))))
	mustRegister(t, parent, Provide(parentSecond,
		func(_ Resolver) (any, error) { return "b", nil },
		WithOnClose(record("parent-second"))))

	child := parent.CreateChild("child")
	childTok := Named[string]("child-only")
	mustRegister(t, child, Provide(childTok,
		func(_ Resolver) (any, error) { return "c", nil },
		WithOnClose(record("child"))))

	// Build every instance so all three hooks are eligible to run.
	for _, get := range []func() (any, error){
		func() (any, error) { return parent.Get(parentFirst) },
		func() (any, error) { return parent.Get(parentSecond) },
		func() (any, error) { return child.Get(childTok) },
	} {
		if _, err := get(); err != nil {
			t.Fatalf("resolve: %v", err)
		}
	}

	if err := parent.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	mu.Lock()
	got := append([]string(nil), order...)
	mu.Unlock()

	want := []string{"child", "parent-second", "parent-first"}
	if len(got) != len(want) {
		t.Fatalf("close order = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("close order[%d] = %q, want %q (full order %v)", i, got[i], want[i], got)
		}
	}
}

// TestClose_SkipsNeverBuiltSingleton asserts a registered-but-never-resolved
// singleton's close hook does not run. Its resource was never acquired — a lazy
// pool nobody opened — so disposing it would close something that does not
// exist.
func TestClose_SkipsNeverBuiltSingleton(t *testing.T) {
	spectest.Proves(t, "go/dependency-injection", "cleanup", "close-skips-never-built-singleton")
	var builtCloses, unbuiltCloses atomic.Int32

	builtTok := Named[*testDB]("built")
	unbuiltTok := Named[*testDB]("never-resolved")

	c := NewContainer("test", nil)
	mustRegister(t, c, Provide(builtTok, func(_ Resolver) (any, error) {
		return &testDB{DSN: "built"}, nil
	}, WithOnClose(func() error { builtCloses.Add(1); return nil })))
	mustRegister(t, c, Provide(unbuiltTok, func(_ Resolver) (any, error) {
		return &testDB{DSN: "never"}, nil
	}, WithOnClose(func() error { unbuiltCloses.Add(1); return nil })))

	if _, err := c.Get(builtTok); err != nil {
		t.Fatalf("resolve built singleton: %v", err)
	}

	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if got := builtCloses.Load(); got != 1 {
		t.Errorf("built singleton close hook ran %d times, want 1", got)
	}
	if got := unbuiltCloses.Load(); got != 0 {
		t.Errorf("never-built singleton close hook ran %d times, want 0", got)
	}
}
