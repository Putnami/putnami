package ctxutil

import (
	"context"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
)

// TestWithRequestTimeout_ZeroLeavesContextUnchanged proves a non-positive
// timeout is a pass-through: the same context is returned and its (absent)
// deadline is untouched, while the returned cancel is still safe to defer.
func TestWithRequestTimeout_ZeroLeavesContextUnchanged(t *testing.T) {
	spectest.Proves(t, "go/bounded-request-contexts", "non-positive-timeout", "non-positive-timeout-passes-the-context-through")
	base := context.Background()
	for _, timeout := range []time.Duration{0, -time.Second} {
		ctx, cancel := WithRequestTimeout(base, timeout)
		if ctx != base {
			t.Fatalf("timeout %v: expected the same context back, got a derived one", timeout)
		}
		if _, ok := ctx.Deadline(); ok {
			t.Fatalf("timeout %v: expected no deadline on the pass-through context", timeout)
		}
		cancel() // must not panic
	}
}

// TestWithRequestTimeout_BoundsPositive proves a positive timeout installs a
// deadline roughly `timeout` from now, so a downstream operation cannot run
// unbounded.
func TestWithRequestTimeout_BoundsPositive(t *testing.T) {
	spectest.Proves(t, "go/bounded-request-contexts", "positive-timeout", "positive-timeout-derives-a-deadline-and-preserves-values")
	type contextKey struct{}
	base := context.WithValue(context.Background(), contextKey{}, "preserved")
	start := time.Now()
	ctx, cancel := WithRequestTimeout(base, 50*time.Millisecond)
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("expected a deadline on the bounded context")
	}
	if d := deadline.Sub(start); d < 40*time.Millisecond || d > 200*time.Millisecond {
		t.Fatalf("deadline %v is not ~50ms from start", d)
	}
	if got := ctx.Value(contextKey{}); got != "preserved" {
		t.Fatalf("context value = %v, want preserved", got)
	}
}

// TestWithRequestTimeout_ReBoundsDetachedContext is the invariant the audit
// guard protects: a context.WithoutCancel context has NO deadline (detaching
// strips it), and WithRequestTimeout must re-install one so a hung dependency
// running on the detached context still fails closed instead of blocking
// forever.
func TestWithRequestTimeout_ReBoundsDetachedContext(t *testing.T) {
	spectest.Proves(t, "go/bounded-request-contexts", "detached-context", "detached-context-is-rebounded")
	parent, cancelParent := context.WithTimeout(context.Background(), time.Hour)
	defer cancelParent()

	detached := context.WithoutCancel(parent)
	if _, ok := detached.Deadline(); ok {
		t.Fatal("precondition: detached context should have no deadline")
	}

	ctx, cancel := WithRequestTimeout(detached, 20*time.Millisecond)
	defer cancel()

	if _, ok := ctx.Deadline(); !ok {
		t.Fatal("expected WithRequestTimeout to re-install a deadline on the detached context")
	}

	select {
	case <-ctx.Done():
		if err := ctx.Err(); err != context.DeadlineExceeded {
			t.Fatalf("expected DeadlineExceeded, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("bounded detached context did not expire — a hung dependency would wedge")
	}
}
