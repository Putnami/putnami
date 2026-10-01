package keyringstore

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/protocol/keyring"
	"go.putnami.dev/protocol/transaction"
)

// fakeRotator is a DB-free Rotator that records how many times it was driven and
// returns a canned outcome. It lets the scheduler contract be tested with no
// database — the whole point of keeping the scheduler DB-agnostic.
type fakeRotator struct {
	calls   atomic.Int32
	outcome transaction.Outcome
}

func (f *fakeRotator) Rotate(_ context.Context, _ string, _ keyring.PrivateJWK) (transaction.Outcome, error) {
	f.calls.Add(1)
	return f.outcome, nil
}

// TestSchedulerFiresPerTickAndStops drives the scheduler with a manual tick
// channel so the firing/stopping contract is DETERMINISTIC (no reliance on real
// time). It asserts the policy — which drives a fake Rotator — fires exactly
// once per tick for N ticks, then stops firing after stop() returns.
func TestSchedulerFiresPerTickAndStops(t *testing.T) {
	store := &fakeRotator{outcome: transaction.OutcomeApplied}
	fired := make(chan struct{}, 1)
	policy := func(ctx context.Context) error {
		if _, err := store.Rotate(ctx, "pred", keyring.PrivateJWK{Kid: "succ"}); err != nil {
			return err
		}
		fired <- struct{}{} // hand back to the test so each tick is observed
		return nil
	}

	ticks := make(chan time.Time)
	sched, err := NewRotationScheduler(time.Hour, policy, withTicks(ticks))
	if err != nil {
		t.Fatalf("NewRotationScheduler: %v", err)
	}
	stop := sched.Start(context.Background())

	const n = 3
	for i := 0; i < n; i++ {
		ticks <- time.Now() // deliver a tick
		<-fired             // wait for the policy invocation it triggered to finish
	}

	stop() // blocks until the loop goroutine has exited

	if got := store.calls.Load(); got != n {
		t.Fatalf("policy fired %d times, want %d", got, n)
	}

	// After stop, the loop is gone: a further tick can never be consumed, so the
	// count stays put. A non-blocking send proves nothing is listening.
	select {
	case ticks <- time.Now():
		t.Fatal("scheduler consumed a tick after stop")
	default:
	}
	if got := store.calls.Load(); got != n {
		t.Fatalf("policy fired again after stop: %d, want %d", got, n)
	}
}

// TestSchedulerStopsOnContextCancel proves cancellation, not just the stop
// function, terminates the loop.
func TestSchedulerStopsOnContextCancel(t *testing.T) {
	store := &fakeRotator{outcome: transaction.OutcomeApplied}
	fired := make(chan struct{}, 1)
	policy := func(ctx context.Context) error {
		_, _ = store.Rotate(ctx, "p", keyring.PrivateJWK{})
		fired <- struct{}{}
		return nil
	}
	ticks := make(chan time.Time)
	sched, err := NewRotationScheduler(time.Hour, policy, withTicks(ticks))
	if err != nil {
		t.Fatalf("NewRotationScheduler: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	stop := sched.Start(ctx)
	defer stop()

	ticks <- time.Now()
	<-fired

	cancel()
	stop() // returns once the canceled loop's goroutine has exited

	if got := store.calls.Load(); got != 1 {
		t.Fatalf("policy fired %d times, want 1 before cancel", got)
	}
}

// TestSchedulerStopIdempotent confirms the returned stop is safe to call
// repeatedly (idempotent cancel + join).
func TestSchedulerStopIdempotent(t *testing.T) {
	spectest.Proves(t, "go/signing-key-rotation", "scheduled-policy", "nothing-fires-after-the-idempotent-stop-returns")
	sched, err := NewRotationScheduler(time.Hour, func(context.Context) error { return nil }, withTicks(make(chan time.Time)))
	if err != nil {
		t.Fatalf("NewRotationScheduler: %v", err)
	}
	stop := sched.Start(context.Background())
	stop()
	stop()
	stop()
}

// TestSchedulerReportsPolicyErrors confirms a policy error reaches the error
// handler and the loop keeps running (a transient failure must not kill the
// schedule).
func TestSchedulerReportsPolicyErrors(t *testing.T) {
	spectest.Proves(t, "go/signing-key-rotation", "scheduled-policy", "a-policy-error-never-stops-future-ticks")
	boom := errors.New("policy failed")
	var got atomic.Pointer[error]
	handled := make(chan struct{}, 1)
	policy := func(context.Context) error { return boom }

	ticks := make(chan time.Time)
	sched, err := NewRotationScheduler(time.Hour, policy,
		withTicks(ticks),
		WithErrorHandler(func(e error) {
			got.Store(&e)
			handled <- struct{}{}
		}))
	if err != nil {
		t.Fatalf("NewRotationScheduler: %v", err)
	}
	stop := sched.Start(context.Background())
	defer stop()

	ticks <- time.Now()
	<-handled

	if p := got.Load(); p == nil || !errors.Is(*p, boom) {
		t.Fatalf("error handler got %v, want %v", p, boom)
	}
	// The loop survives the error: a second tick still fires.
	ticks <- time.Now()
	<-handled
}

// TestNewRotationSchedulerValidation pins the fail-closed construction contract.
func TestNewRotationSchedulerValidation(t *testing.T) {
	spectest.Proves(t, "go/signing-key-rotation", "scheduled-policy", "only-a-caller-supplied-policy-is-ever-invoked")
	if _, err := NewRotationScheduler(0, func(context.Context) error { return nil }); err == nil {
		t.Fatal("expected error for non-positive interval")
	}
	if _, err := NewRotationScheduler(time.Second, nil); err == nil {
		t.Fatal("expected error for nil policy (scheduler must never rotate on its own)")
	}
}

// TestSchedulerNeverOverlapsInvocations pins the "never overlaps invocations"
// clause structurally: the loop goroutine calls the policy synchronously, so a
// tick delivered while the policy is still running has no listener. A scheduler
// that dispatched the policy on its own goroutine would be back at the select
// and would consume it — which is exactly what the non-blocking send detects.
func TestSchedulerNeverOverlapsInvocations(t *testing.T) {
	spectest.Proves(t, "go/signing-key-rotation", "scheduled-policy", "policy-invocations-never-overlap")

	entered := make(chan struct{})
	release := make(chan struct{})
	policy := func(context.Context) error {
		entered <- struct{}{}
		<-release
		return nil
	}

	ticks := make(chan time.Time)
	sched, err := NewRotationScheduler(time.Hour, policy, withTicks(ticks))
	if err != nil {
		t.Fatalf("NewRotationScheduler: %v", err)
	}
	stop := sched.Start(context.Background())

	ticks <- time.Now() // first invocation begins…
	<-entered           // …and is now blocked inside the policy

	select {
	case ticks <- time.Now():
		t.Fatal("a second tick was consumed while the policy was still running: invocations can overlap")
	default:
	}

	release <- struct{}{} // let the first invocation finish

	ticks <- time.Now() // now the loop is listening again
	<-entered
	release <- struct{}{}

	stop()
}
