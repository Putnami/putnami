package database

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	perrors "go.putnami.dev/errors"
	"go.putnami.dev/protocol/features/spectest"
)

// contextPoolFactory is a physical-open seam that announces each open with the
// context it received on entered, then blocks on release, so a test can end a
// caller's context while the shared open is in flight and observe the context
// the open itself runs under.
type contextPoolFactory struct {
	entered chan context.Context
	release chan struct{}
}

func (f *contextPoolFactory) New(ctx context.Context, _ PoolConfig) (*pgxpool.Pool, error) {
	f.entered <- ctx
	<-f.release
	return nil, nil
}

// newContextTestPools builds a registry over one named datasource and one
// default datasource, wired to a contextPoolFactory.
func newContextTestPools(t *testing.T) (*Pools, *contextPoolFactory) {
	t.Helper()
	t.Setenv(EnvBinding, multiBinding)
	f := &contextPoolFactory{entered: make(chan context.Context, 4), release: make(chan struct{})}
	p := NewPlugin(PluginConfig{Datasource: "platform_core", Datasources: []DatasourceConfig{{Name: "platform_iam"}}})
	p.newPhysical = f.New
	pools, err := p.buildPools()
	if err != nil {
		t.Fatalf("buildPools: %v", err)
	}
	return pools, f
}

// awaitOpen waits for the factory to receive one physical open and returns
// the context the open runs under.
func awaitOpen(t *testing.T, f *contextPoolFactory) context.Context {
	t.Helper()
	select {
	case ctx := <-f.entered:
		return ctx
	case <-time.After(2 * time.Second):
		t.Fatal("no physical open reached the factory")
		return nil
	}
}

type poolResult struct {
	pool *Pool
	err  error
}

func awaitResult(t *testing.T, done <-chan poolResult, what string) poolResult {
	t.Helper()
	select {
	case r := <-done:
		return r
	case <-time.After(2 * time.Second):
		t.Fatalf("%s did not return", what)
		return poolResult{}
	}
}

// TestPools_ForContext_ReturnsWhenTheCallerContextEnds proves a caller whose
// context ends while the first open is in flight returns at once with a
// db.connection error wrapping ctx.Err(), while the shared open keeps running
// under ConnectTimeout and caches the pool for the next caller.
func TestPools_ForContext_ReturnsWhenTheCallerContextEnds(t *testing.T) {
	spectest.Proves(t, "go/shared-datasource-pooling", "caller-bounded-first-open", "a-canceled-caller-returns-with-its-context-error")
	spectest.Proves(t, "go/shared-datasource-pooling", "caller-bounded-first-open", "the-shared-open-outlives-a-canceled-caller")
	pools, f := newContextTestPools(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan poolResult, 1)
	go func() {
		pool, err := pools.ForContext(ctx, "platform_iam")
		done <- poolResult{pool, err}
	}()
	openCtx := awaitOpen(t, f)
	cancel()

	r := awaitResult(t, done, "ForContext after its context ended")
	if r.pool != nil {
		t.Fatalf("ForContext returned a pool after its context ended")
	}
	if !errors.Is(r.err, context.Canceled) {
		t.Fatalf("ForContext error = %v, want one wrapping context.Canceled", r.err)
	}
	if !perrors.Is(r.err, CodeConnection) {
		t.Fatalf("ForContext error = %v, want code %s", r.err, CodeConnection)
	}
	if !slices.Contains(perrors.GetAttrs(r.err), perrors.String("datasource", "platform_iam")) {
		t.Errorf("ForContext error attrs = %v, want datasource=platform_iam", perrors.GetAttrs(r.err))
	}

	if openCtx.Err() != nil {
		t.Fatalf("the shared open's context ended with the caller's: %v", openCtx.Err())
	}
	if _, ok := openCtx.Deadline(); !ok {
		t.Error("the shared open runs without a deadline, want ConnectTimeout")
	}

	close(f.release)
	pool, err := pools.For("platform_iam")
	if err != nil {
		t.Fatalf("For after the shared open finished: %v", err)
	}
	if pool == nil {
		t.Fatal("For after the shared open finished returned no pool")
	}
	select {
	case <-f.entered:
		t.Fatal("a second physical open reached the factory: the shared open must cache its pool")
	default:
	}
}

// TestPools_ForContext_CancelledFirstCallerDoesNotFailTheOthers proves the
// caller that started the open can leave without failing a caller that waits
// on the same open.
func TestPools_ForContext_CancelledFirstCallerDoesNotFailTheOthers(t *testing.T) {
	spectest.Proves(t, "go/shared-datasource-pooling", "caller-bounded-first-open", "the-shared-open-outlives-a-canceled-caller")
	pools, f := newContextTestPools(t)

	firstCtx, cancelFirst := context.WithCancel(context.Background())
	first := make(chan poolResult, 1)
	go func() {
		pool, err := pools.ForContext(firstCtx, "platform_iam")
		first <- poolResult{pool, err}
	}()
	awaitOpen(t, f)

	second := make(chan poolResult, 1)
	go func() {
		pool, err := pools.ForContext(context.Background(), "platform_iam")
		second <- poolResult{pool, err}
	}()

	cancelFirst()
	if r := awaitResult(t, first, "the canceled first caller"); !errors.Is(r.err, context.Canceled) {
		t.Fatalf("first caller error = %v, want context.Canceled", r.err)
	}
	select {
	case r := <-second:
		t.Fatalf("the waiting caller returned before the open finished: %+v", r)
	case <-time.After(20 * time.Millisecond):
	}

	close(f.release)
	r := awaitResult(t, second, "the waiting caller")
	if r.err != nil || r.pool == nil {
		t.Fatalf("waiting caller = (%v, %v), want the pool", r.pool, r.err)
	}
}

// TestPools_DefaultContext_ReturnsAtTheCallerDeadline proves the default
// datasource lookup is bounded by the caller's deadline the same way.
func TestPools_DefaultContext_ReturnsAtTheCallerDeadline(t *testing.T) {
	spectest.Proves(t, "go/shared-datasource-pooling", "caller-bounded-first-open", "a-canceled-caller-returns-with-its-context-error")
	pools, f := newContextTestPools(t)
	defer close(f.release)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	pool, err := pools.DefaultContext(ctx)
	if pool != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("DefaultContext = (%v, %v), want an error wrapping context.DeadlineExceeded", pool, err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("DefaultContext returned after %s, want it bounded by the 20ms deadline", elapsed)
	}
}

// TestPools_HealthCheck_BoundedByTheProbeContext proves a probe that opens
// the default datasource returns when the probe's context ends.
func TestPools_HealthCheck_BoundedByTheProbeContext(t *testing.T) {
	pools, f := newContextTestPools(t)
	defer close(f.release)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := pools.HealthCheck(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("HealthCheck = %v, want an error wrapping context.DeadlineExceeded", err)
	}
}
