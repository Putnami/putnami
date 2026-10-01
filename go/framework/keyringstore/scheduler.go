package keyringstore

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.putnami.dev/protocol/keyring"
	"go.putnami.dev/protocol/transaction"
)

// Rotator is the store capability a scheduler policy typically drives.
// *DBKeyringStore satisfies it, and so can a test fake, which keeps the
// scheduler itself DB-agnostic — it never touches a database, it only invokes a
// caller-supplied policy on a schedule.
type Rotator interface {
	Rotate(ctx context.Context, predecessorKid string, successor keyring.PrivateJWK) (transaction.Outcome, error)
}

// RotationFunc is the caller-supplied rotation POLICY the scheduler invokes on
// each tick. The policy decides whether and how to rotate — e.g. inspect the
// active key's age, then call a Rotator.Rotate — and is the ONLY thing that ever
// triggers a rotation. The scheduler never rotates on its own. A policy error is
// returned to the loop, which reports it through the optional OnError hook and
// continues on the next tick (a transient failure must not kill the schedule).
type RotationFunc func(ctx context.Context) error

// RotationScheduler periodically invokes a rotation policy until it is stopped
// or its context is canceled. It carries no database dependency; it is a pure
// timer wrapped around a policy, so it is fully testable with a fake Rotator.
type RotationScheduler struct {
	interval time.Duration
	policy   RotationFunc
	onError  func(error)

	// ticks, when non-nil, replaces the real time.Ticker as the tick source.
	// It is an unexported test seam so the firing/stopping contract can be
	// exercised deterministically without real time passing.
	ticks <-chan time.Time
}

// SchedulerOption configures a RotationScheduler.
type SchedulerOption func(*RotationScheduler)

// WithErrorHandler installs a callback invoked with any error the policy
// returns. It must not log key material (the policy controls what it surfaces).
// Without it, policy errors are swallowed and the loop continues.
func WithErrorHandler(fn func(error)) SchedulerOption {
	return func(s *RotationScheduler) { s.onError = fn }
}

// withTicks overrides the tick source (test-only, deterministic).
func withTicks(ch <-chan time.Time) SchedulerOption {
	return func(s *RotationScheduler) { s.ticks = ch }
}

// NewRotationScheduler builds a scheduler that invokes policy every interval. It
// fails closed: a non-positive interval or a nil policy is an error, so a
// scheduler can never be constructed in a state where it would rotate on its own
// (no policy) or spin (no interval).
func NewRotationScheduler(interval time.Duration, policy RotationFunc, opts ...SchedulerOption) (*RotationScheduler, error) {
	if interval <= 0 {
		return nil, fmt.Errorf("keyringstore: rotation interval must be positive, got %s", interval)
	}
	if policy == nil {
		return nil, fmt.Errorf("keyringstore: rotation scheduler requires a policy (it never rotates on its own)")
	}
	s := &RotationScheduler{interval: interval, policy: policy}
	for _, o := range opts {
		o(s)
	}
	return s, nil
}

// Start launches the rotation loop in a background goroutine and returns a stop
// function. The loop invokes the policy once per tick until ctx is canceled or
// stop is called. The returned stop is idempotent (safe to call any number of
// times) and BLOCKS until the loop goroutine has fully exited, so after stop
// returns the policy is guaranteed not to fire again. The loop drops any tick
// that arrives while a policy invocation is in flight (no overlapping
// rotations).
func (s *RotationScheduler) Start(ctx context.Context) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	ticks := s.ticks
	stopTicker := func() {}
	if ticks == nil {
		t := time.NewTicker(s.interval)
		ticks = t.C
		stopTicker = t.Stop
	}

	go func() {
		defer close(done)
		defer stopTicker()
		for {
			select {
			case <-ctx.Done():
				return
			case _, ok := <-ticks:
				if !ok {
					return
				}
				if err := s.policy(ctx); err != nil && s.onError != nil {
					s.onError(err)
				}
			}
		}
	}()

	var once sync.Once
	return func() {
		once.Do(cancel)
		<-done
	}
}
