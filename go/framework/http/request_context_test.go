package http

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
)

func TestRequestContextProbesTheCancellationKey(t *testing.T) {
	if cancelCtxKey == nil {
		t.Fatal("the probe found no cancellation key: every request would arm its timer")
	}
}

func TestRequestContextEndsAtItsDeadlineWithoutAWaiter(t *testing.T) {
	ctx := newRequestContext(context.Background(), 20*time.Millisecond)
	defer ctx.cancel()
	if err := ctx.Err(); err != nil {
		t.Fatalf("Err() = %v before the deadline, want nil", err)
	}
	if ctx.inner.Load() != nil {
		t.Fatal("Err armed the context before the deadline")
	}
	time.Sleep(30 * time.Millisecond)
	if err := ctx.Err(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Err() = %v past the deadline, want context.DeadlineExceeded", err)
	}
	if err := context.Cause(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Cause() = %v past the deadline, want context.DeadlineExceeded", err)
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("Done() is open past the deadline")
	}
}

func TestRequestContextWakesAWaiterAtItsDeadline(t *testing.T) {
	ctx := newRequestContext(context.Background(), 20*time.Millisecond)
	defer ctx.cancel()
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done() did not close at the deadline")
	}
	if err := ctx.Err(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Err() = %v, want context.DeadlineExceeded", err)
	}
}

func TestRequestContextEndsADerivedContextAtItsDeadline(t *testing.T) {
	ctx := newRequestContext(context.Background(), 20*time.Millisecond)
	defer ctx.cancel()
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	select {
	case <-child.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the derived context did not end at the deadline")
	}
	if err := child.Err(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("derived Err() = %v, want context.DeadlineExceeded", err)
	}
}

func TestRequestContextFollowsItsParent(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	ctx := newRequestContext(parent, time.Minute)
	defer ctx.cancel()
	cancelParent()
	if err := ctx.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Err() = %v after the parent ended, want context.Canceled", err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done() did not close after the parent ended")
	}
}

func TestRequestContextCancelEndsItBeforeAndAfterArming(t *testing.T) {
	unarmed := newRequestContext(context.Background(), time.Minute)
	unarmed.cancel()
	if err := unarmed.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Err() = %v after cancel, want context.Canceled", err)
	}
	select {
	case <-unarmed.Done():
	default:
		t.Fatal("Done() is open after cancel")
	}

	armed := newRequestContext(context.Background(), time.Minute)
	done := armed.Done()
	armed.cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Done() did not close on cancel")
	}
	if err := armed.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Err() = %v after cancel, want context.Canceled", err)
	}
}

func TestRequestContextKeepsAnEarlierParentDeadline(t *testing.T) {
	parent, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	want, _ := parent.Deadline()
	ctx := newRequestContext(parent, time.Hour)
	defer ctx.cancel()
	if got, ok := ctx.Deadline(); !ok || !got.Equal(want) {
		t.Fatalf("Deadline() = %v, %v, want the parent's %v", got, ok, want)
	}
}

func TestRequestContextResolvesParentValues(t *testing.T) {
	type key struct{}
	parent := context.WithValue(context.Background(), key{}, "value")
	ctx := newRequestContext(parent, time.Minute)
	defer ctx.cancel()
	if got := ctx.Value(key{}); got != "value" {
		t.Fatalf("Value() = %v, want the parent's value", got)
	}
	if ctx.inner.Load() != nil {
		t.Fatal("a value lookup armed the context")
	}
}

func TestRequestContextCanceledBeforeItsDeadlineStaysCanceled(t *testing.T) {
	ctx := newRequestContext(context.Background(), 10*time.Millisecond)
	ctx.cancel()
	time.Sleep(20 * time.Millisecond)
	if err := ctx.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Err() = %v past the deadline after cancel, want context.Canceled", err)
	}
}

func TestRequestContextDerivedContextsStartNoGoroutine(t *testing.T) {
	ctx := newRequestContext(context.Background(), time.Minute)
	defer ctx.cancel()
	before := runtime.NumGoroutine()
	cancels := make([]context.CancelFunc, 0, 100)
	for range 100 {
		_, cancel := context.WithCancel(ctx)
		cancels = append(cancels, cancel)
	}
	after := runtime.NumGoroutine()
	for _, cancel := range cancels {
		cancel()
	}
	if after-before >= 50 {
		t.Fatalf("100 derived contexts started %d goroutines: the cancellation key probe found the wrong key", after-before)
	}
}

func TestRequestContextCanceledAfterItsDeadlineKeepsDeadlineExceeded(t *testing.T) {
	ctx := newRequestContext(context.Background(), 10*time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	ctx.cancel()
	if err := ctx.Err(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Err() = %v after a cancel past the deadline, want context.DeadlineExceeded", err)
	}
}

func TestRequestContextCanceledBeforeItsParentEndsStaysCanceled(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	ctx := newRequestContext(parent, time.Minute)
	ctx.cancel()
	cancelParent()
	if err := ctx.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Err() = %v after a cancel before the parent's deadline, want context.Canceled", err)
	}
}

func TestRequestContextParentEndedThenCanceledKeepsTheParentEnd(t *testing.T) {
	parent, cancelParent := context.WithCancelCause(context.Background())
	ctx := newRequestContext(parent, time.Minute)
	cancelParent(nil)
	ctx.cancel()
	if err := ctx.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Err() = %v after the parent ended, want context.Canceled", err)
	}
}

func TestRequestContextPastItsDeadlineWhenTheParentEndsEndsWithDeadlineExceeded(t *testing.T) {
	spectest.Proves(t, "go/http-services", "caller-withdrawal", "a-stalled-request-ends-on-its-deadline-at-error")
	parent, cancelParent := context.WithCancel(context.Background())
	ctx := newRequestContext(parent, 10*time.Millisecond)
	defer ctx.cancel()
	time.Sleep(20 * time.Millisecond)
	cancelParent()
	if err := ctx.Err(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Err() = %v past the deadline after the parent ended, want context.DeadlineExceeded", err)
	}
	if err := context.Cause(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Cause() = %v, want context.DeadlineExceeded", err)
	}
}

func TestRequestContextCanceledBeforeAnEarlierParentDeadlineStaysCanceled(t *testing.T) {
	parent, cancelParent := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancelParent()
	ctx := newRequestContext(parent, time.Hour)
	ctx.cancel()
	<-parent.Done()
	if err := ctx.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Err() = %v after a cancel before the parent's deadline, want context.Canceled", err)
	}
}

func TestRequestContextParentEndReadBeforeTheDeadlineKeepsCanceled(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	ctx := newRequestContext(parent, 10*time.Millisecond)
	defer ctx.cancel()
	cancelParent()
	if err := ctx.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Err() = %v after the parent ended, want context.Canceled", err)
	}
	time.Sleep(20 * time.Millisecond)
	if err := ctx.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Err() = %v past the deadline, want the context.Canceled read before it", err)
	}
}

func TestRequestContextParentEndFirstReadPastTheDeadlineEndsWithDeadlineExceeded(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	ctx := newRequestContext(parent, 10*time.Millisecond)
	defer ctx.cancel()
	cancelParent()
	time.Sleep(20 * time.Millisecond)
	if err := ctx.Err(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Err() = %v first read past the deadline, want context.DeadlineExceeded as ADR 0005 states", err)
	}
}

func TestRequestContextParentEndFirstWaitPastTheDeadlineEndsWithDeadlineExceeded(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	ctx := newRequestContext(parent, 10*time.Millisecond)
	defer ctx.cancel()
	cancelParent()
	time.Sleep(20 * time.Millisecond)
	<-ctx.Done()
	if err := ctx.Err(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Err() = %v armed by a wait past the deadline, want context.DeadlineExceeded as ADR 0005 states", err)
	}
}
