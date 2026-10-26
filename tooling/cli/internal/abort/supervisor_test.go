package abort

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
)

// fakeParent is a parent pid a test changes.
type fakeParent struct{ pid atomic.Int64 }

func (p *fakeParent) getppid() int { return int(p.pid.Load()) }

// A change of the parent pid cancels the watched context and records the
// stop as a supervisor's.
func TestWatchSupervisorCancelsOnceTheParentPIDChanges(t *testing.T) {
	spectest.Proves(t, "cli/supervised-serve-run", "a-lost-supervisor-stops-the-run", "a-changed-parent-pid-cancels-the-run")
	Reset()
	t.Cleanup(Reset)
	parent := &fakeParent{}
	parent.pid.Store(4242)
	ctx, stop := watchSupervisor(context.Background(), time.Millisecond, parent.getppid)
	defer stop()
	select {
	case <-ctx.Done():
		t.Fatalf("the watch canceled while the parent ran: %v", ctx.Err())
	case <-time.After(50 * time.Millisecond):
	}
	if got := Source(); got != "" {
		t.Fatalf("Source() = %q while the parent ran, want empty", got)
	}
	parent.pid.Store(1)
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the watch did not cancel once the parent pid changed")
	}
	if got := Source(); got != SourceSignal {
		t.Errorf("Source() = %q, want %q", got, SourceSignal)
	}
}

// Ending the watch, or canceling the context it copies, records no stop.
func TestWatchSupervisorEndsWithoutALoss(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  func(stop, cancelParent context.CancelFunc)
	}{
		{"stopped", func(stop, _ context.CancelFunc) { stop() }},
		{"parent context canceled", func(_, cancelParent context.CancelFunc) { cancelParent() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			Reset()
			t.Cleanup(Reset)
			parent := &fakeParent{}
			parent.pid.Store(4242)
			base, cancelParent := context.WithCancel(context.Background())
			defer cancelParent()
			ctx, stop := watchSupervisor(base, time.Millisecond, parent.getppid)
			defer stop()
			tc.end(stop, cancelParent)
			<-ctx.Done()
			if !errors.Is(ctx.Err(), context.Canceled) {
				t.Errorf("ctx.Err() = %v, want context.Canceled", ctx.Err())
			}
			// A parent change after the end is no longer watched.
			parent.pid.Store(1)
			time.Sleep(20 * time.Millisecond)
			if got := Source(); got != "" {
				t.Errorf("Source() = %q, want empty", got)
			}
		})
	}
}
