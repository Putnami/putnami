package parent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
)

const (
	workloadFeature   = "tooling/workload-qualification"
	parentRequirement = "a-lost-putnami-tears-the-command-down"
)

// fakeParent is a parent pid a test changes.
type fakeParent struct{ pid atomic.Int64 }

func (p *fakeParent) getppid() int { return int(p.pid.Load()) }

// A change of the parent pid cancels the watched context with ErrGone.
func TestWatchCancelsOnceTheParentPIDChanges(t *testing.T) {
	spectest.Proves(t, workloadFeature, parentRequirement, "a-changed-parent-pid-cancels-the-command")
	parent := &fakeParent{}
	parent.pid.Store(4242)
	ctx, stop := watch(context.Background(), time.Millisecond, parent.getppid)
	defer stop()

	select {
	case <-ctx.Done():
		t.Fatalf("the watch canceled while the parent ran: %v", context.Cause(ctx))
	case <-time.After(50 * time.Millisecond):
	}
	parent.pid.Store(1)
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the watch did not cancel once the parent pid changed")
	}
	if cause := context.Cause(ctx); !errors.Is(cause, ErrGone) {
		t.Errorf("cause = %v, want ErrGone", cause)
	}
}

// Ending the watch cancels its context without ErrGone, and a canceled parent
// context cancels the watch the same way.
func TestWatchEndsWithoutALoss(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  func(stop, cancelParent context.CancelFunc)
	}{
		{"stopped", func(stop, _ context.CancelFunc) { stop() }},
		{"parent context canceled", func(_, cancelParent context.CancelFunc) { cancelParent() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent := &fakeParent{}
			parent.pid.Store(4242)
			base, cancelParent := context.WithCancel(context.Background())
			defer cancelParent()
			ctx, stop := watch(base, time.Millisecond, parent.getppid)
			defer stop()
			tc.end(stop, cancelParent)
			<-ctx.Done()
			if cause := context.Cause(ctx); errors.Is(cause, ErrGone) || !errors.Is(cause, context.Canceled) {
				t.Errorf("cause = %v, want context.Canceled", cause)
			}
		})
	}
}
