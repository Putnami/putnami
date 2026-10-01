package parallel

import (
	"context"
	stderrors "errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
)

func TestMapBounded_EmptyItems(t *testing.T) {
	results, err := MapBounded(context.Background(), []int(nil), 4, func(_ context.Context, i int) (int, error) {
		return i, nil
	})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if results != nil {
		t.Fatalf("results = %v, want nil", results)
	}
}

func TestMapBounded_OrderedResults(t *testing.T) {
	spectest.Proves(t, "go/bounded-parallel-work", "ordering", "ordered-results")
	items := []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	results, err := MapBounded(context.Background(), items, 3, func(_ context.Context, i int) (int, error) {
		// Sleep with reversed proportionality to amplify any reordering.
		time.Sleep(time.Duration(11-i) * time.Millisecond)
		return i * 10, nil
	})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	want := []int{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}
	if len(results) != len(want) {
		t.Fatalf("len(results) = %d, want %d", len(results), len(want))
	}
	for i, got := range results {
		if got != want[i] {
			t.Errorf("results[%d] = %d, want %d", i, got, want[i])
		}
	}
}

func TestMapBounded_BoundedInflight(t *testing.T) {
	spectest.Proves(t, "go/bounded-parallel-work", "concurrency", "bounded-inflight")
	const items = 50
	const limit = 4
	var (
		inflight    atomic.Int32
		maxInflight atomic.Int32
	)

	work := make([]int, items)
	for i := range work {
		work[i] = i
	}

	_, err := MapBounded(context.Background(), work, limit, func(_ context.Context, _ int) (int, error) {
		cur := inflight.Add(1)
		defer inflight.Add(-1)
		for {
			peak := maxInflight.Load()
			if cur <= peak || maxInflight.CompareAndSwap(peak, cur) {
				break
			}
		}
		time.Sleep(2 * time.Millisecond)
		return 0, nil
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got := maxInflight.Load(); got > limit {
		t.Errorf("max in-flight = %d, want <= %d", got, limit)
	}
}

func TestMapBounded_UnboundedWhenLimitZero(t *testing.T) {
	spectest.Proves(t, "go/bounded-parallel-work", "concurrency", "unbounded-when-limit-zero")
	const items = 16
	var (
		inflight atomic.Int32
		maxSeen  atomic.Int32
	)
	release := make(chan struct{})

	// Releaser closes the gate once every worker has reported its arrival.
	go func() {
		for inflight.Load() < items {
			time.Sleep(time.Millisecond)
		}
		close(release)
	}()

	work := make([]int, items)
	_, err := MapBounded(context.Background(), work, 0, func(_ context.Context, _ int) (int, error) {
		cur := inflight.Add(1)
		for {
			peak := maxSeen.Load()
			if cur <= peak || maxSeen.CompareAndSwap(peak, cur) {
				break
			}
		}
		<-release
		return 0, nil
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got := maxSeen.Load(); int(got) < items {
		t.Fatalf("limit=0 should run all items concurrently, max in-flight = %d, want %d", got, items)
	}
}

func TestMapBounded_FirstErrorWins(t *testing.T) {
	spectest.Proves(t, "go/bounded-parallel-work", "failure", "first-error-wins")
	errBoom := stderrors.New("boom")
	const items = 20
	work := make([]int, items)
	for i := range work {
		work[i] = i
	}

	results, err := MapBounded(context.Background(), work, 4, func(_ context.Context, i int) (int, error) {
		if i == 3 {
			return 0, errBoom
		}
		time.Sleep(5 * time.Millisecond)
		return i, nil
	})
	if !stderrors.Is(err, errBoom) {
		t.Fatalf("err = %v, want %v", err, errBoom)
	}
	if results != nil {
		t.Errorf("results = %v, want nil on error", results)
	}
}

func TestMapBounded_PositiveLimitStopsDispatchAfterError(t *testing.T) {
	spectest.Proves(t, "go/bounded-parallel-work", "failure", "bounded-dispatch-stops-on-cancellation")
	errBoom := stderrors.New("boom")
	work := []int{0, 1, 2, 3, 4}
	var calls atomic.Int32

	results, err := MapBounded(context.Background(), work, 1, func(_ context.Context, i int) (int, error) {
		calls.Add(1)
		if i == 0 {
			return 0, errBoom
		}
		return i, nil
	})
	if !stderrors.Is(err, errBoom) {
		t.Fatalf("err = %v, want %v", err, errBoom)
	}
	if results != nil {
		t.Fatalf("results = %v, want nil on error", results)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("fn calls = %d, want 1; bounded dispatch continued after cancellation", got)
	}
}

func TestCallWorkerIfActive_CanceledGoroutineSkipsUserFunction(t *testing.T) {
	spectest.Proves(t, "go/bounded-parallel-work", "failure", "canceled-goroutine-skips-user-function")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var calls atomic.Int32
	type workerResult struct {
		invoked bool
		err     error
	}
	done := make(chan workerResult, 1)

	// The unbounded dispatcher may already have created this goroutine when a
	// sibling cancels the group. The common pre-invocation guard must prevent
	// it from entering caller-owned work once cancellation is visible.
	go func() {
		_, invoked, err := callWorkerIfActive(ctx, 1, func(_ context.Context, i int) (int, error) {
			calls.Add(1)
			return i, nil
		})
		done <- workerResult{invoked: invoked, err: err}
	}()

	got := <-done
	if got.err != nil {
		t.Fatalf("guard error = %v, want nil", got.err)
	}
	if got.invoked {
		t.Fatal("guard reported user function invocation on canceled context")
	}
	if gotCalls := calls.Load(); gotCalls != 0 {
		t.Fatalf("fn calls = %d, want 0", gotCalls)
	}
}

func TestMapBounded_FirstErrorCancelsSiblings(t *testing.T) {
	spectest.Proves(t, "go/bounded-parallel-work", "failure", "first-error-cancels-siblings")
	errBoom := stderrors.New("boom")
	const (
		items = 32
		limit = 8
	)
	work := make([]int, items)
	for i := range work {
		work[i] = i
	}

	var canceled atomic.Int32
	// The erroring worker waits on this barrier until the rest of the first
	// concurrency batch is parked in its select before it returns. Without it
	// the error can cancel the group before any sibling reaches its select; the
	// siblings then early-return on the already-canceled context (MapBounded
	// skips fn when groupCtx is done) and never observe cancellation, so the
	// assertion raced on goroutine scheduling and flaked under load / many cores.
	entered := make(chan struct{}, items)

	_, err := MapBounded(context.Background(), work, limit, func(ctx context.Context, i int) (int, error) {
		if i == 1 {
			// Items 0..limit-1 form the first in-flight batch; every member but
			// this one signals once it is parked below.
			for range limit - 1 {
				<-entered
			}
			return 0, errBoom
		}
		entered <- struct{}{}
		select {
		case <-time.After(10 * time.Second): // never fires: the group is canceled first
			return i, nil
		case <-ctx.Done():
			canceled.Add(1)
			return 0, ctx.Err()
		}
	})
	if !stderrors.Is(err, errBoom) {
		t.Fatalf("err = %v, want %v", err, errBoom)
	}
	if canceled.Load() == 0 {
		t.Error("no sibling saw ctx cancellation; first error did not cancel group")
	}
}

func TestMapBounded_ParentCancellationStopsDispatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	work := make([]int, 100)
	started := make(chan struct{}, len(work))

	go func() {
		// Cancel after a few items have started.
		for range 4 {
			<-started
		}
		cancel()
	}()

	_, err := MapBounded(ctx, work, 2, func(c context.Context, _ int) (int, error) {
		started <- struct{}{}
		select {
		case <-time.After(50 * time.Millisecond):
			return 0, nil
		case <-c.Done():
			return 0, c.Err()
		}
	})
	if !stderrors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want %v", err, context.Canceled)
	}
}

func TestMapBounded_PreCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var calls atomic.Int32
	_, err := MapBounded(ctx, []int{1, 2, 3}, 2, func(_ context.Context, _ int) (int, error) {
		calls.Add(1)
		return 0, nil
	})
	if !stderrors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want %v", err, context.Canceled)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("fn called %d times on pre-canceled ctx, want 0", got)
	}
}

func TestMapBounded_PropagatesContextValues(t *testing.T) {
	type ctxKey struct{}
	ctx := context.WithValue(context.Background(), ctxKey{}, "hello")
	results, err := MapBounded(ctx, []int{1, 2, 3}, 2, func(c context.Context, _ int) (string, error) {
		v, _ := c.Value(ctxKey{}).(string)
		return v, nil
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	for i, got := range results {
		if got != "hello" {
			t.Errorf("results[%d] = %q, want %q", i, got, "hello")
		}
	}
}

func TestMapBounded_RunsAllItemsExactlyOnce(t *testing.T) {
	const items = 200
	work := make([]int, items)
	for i := range work {
		work[i] = i
	}

	var (
		seenMu sync.Mutex
		seen   = make(map[int]int)
	)
	results, err := MapBounded(context.Background(), work, 8, func(_ context.Context, i int) (int, error) {
		seenMu.Lock()
		seen[i]++
		seenMu.Unlock()
		return i, nil
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(results) != items {
		t.Fatalf("len(results) = %d, want %d", len(results), items)
	}
	for i := range items {
		if seen[i] != 1 {
			t.Errorf("item %d seen %d times, want 1", i, seen[i])
		}
		if results[i] != i {
			t.Errorf("results[%d] = %d, want %d", i, results[i], i)
		}
	}
}

func TestEachBounded_Success(t *testing.T) {
	var sum atomic.Int64
	items := []int64{1, 2, 3, 4, 5}
	err := EachBounded(context.Background(), items, 2, func(_ context.Context, i int64) error {
		sum.Add(i)
		return nil
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got := sum.Load(); got != 15 {
		t.Errorf("sum = %d, want 15", got)
	}
}

func TestEachBounded_FirstErrorWins(t *testing.T) {
	errBoom := stderrors.New("boom")
	err := EachBounded(context.Background(), []int{1, 2, 3, 4, 5}, 2, func(_ context.Context, i int) error {
		if i == 2 {
			return errBoom
		}
		time.Sleep(5 * time.Millisecond)
		return nil
	})
	if !stderrors.Is(err, errBoom) {
		t.Fatalf("err = %v, want %v", err, errBoom)
	}
}

func TestEachBounded_EmptyItems(t *testing.T) {
	if err := EachBounded(context.Background(), []int(nil), 4, func(_ context.Context, _ int) error {
		t.Fatal("fn should not be called")
		return nil
	}); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
}

func TestMapBounded_RecoversWorkerPanic(t *testing.T) {
	spectest.Proves(t, "go/bounded-parallel-work", "panic", "panic-returns-panic-error")
	results, err := MapBounded(context.Background(), []int{1, 2, 3}, 2, func(_ context.Context, i int) (int, error) {
		if i == 2 {
			panic("boom")
		}
		return i, nil
	})
	var pe *PanicError
	if !stderrors.As(err, &pe) {
		t.Fatalf("err = %v (%T), want *PanicError", err, err)
	}
	if pe.Value != "boom" {
		t.Errorf("PanicError.Value = %v, want %q", pe.Value, "boom")
	}
	if len(pe.Stack) == 0 {
		t.Error("PanicError.Stack is empty, want captured stack")
	}
	if results != nil {
		t.Errorf("results = %v, want nil on panic", results)
	}
}

func TestMapBounded_PanicCancelsSiblings(t *testing.T) {
	spectest.Proves(t, "go/bounded-parallel-work", "panic", "panic-cancels-siblings")
	const items = 8
	work := make([]int, items)
	for i := range work {
		work[i] = i
	}
	var (
		arrived  atomic.Int32
		canceled atomic.Int32
	)
	// limit=0 runs every item concurrently; the panicking worker waits until
	// all siblings are parked in their select before panicking, so the group
	// cancellation has an observable effect rather than racing the per-worker
	// "already canceled" guard.
	_, err := MapBounded(context.Background(), work, 0, func(ctx context.Context, i int) (int, error) {
		if i == 0 {
			for arrived.Load() < items-1 {
				time.Sleep(time.Millisecond)
			}
			panic("boom")
		}
		arrived.Add(1)
		select {
		case <-time.After(2 * time.Second):
			return i, nil
		case <-ctx.Done():
			canceled.Add(1)
			return 0, ctx.Err()
		}
	})
	var pe *PanicError
	if !stderrors.As(err, &pe) {
		t.Fatalf("err = %v, want *PanicError", err)
	}
	if got := canceled.Load(); int(got) != items-1 {
		t.Errorf("canceled = %d, want %d (every sibling should observe cancellation)", got, items-1)
	}
}

func TestMapBounded_PanicErrorUnwrapsErrorValue(t *testing.T) {
	sentinel := stderrors.New("sentinel")
	_, err := MapBounded(context.Background(), []int{1}, 1, func(_ context.Context, _ int) (int, error) {
		panic(sentinel)
	})
	if !stderrors.Is(err, sentinel) {
		t.Fatalf("errors.Is(err, sentinel) = false, err = %v", err)
	}
	var pe *PanicError
	if !stderrors.As(err, &pe) {
		t.Fatalf("err = %v, want *PanicError", err)
	}
}

func TestPanicError_Error(t *testing.T) {
	// The formatted message is effectively part of the public contract: it is
	// what surfaces when a recovered-panic error is logged or printed with
	// %v/%s. Pin both the fixed prefix and the interpolated panic value.
	t.Run("string value", func(t *testing.T) {
		pe := &PanicError{Value: "boom"}
		got := pe.Error()
		want := "parallel: worker panicked: boom"
		if got != want {
			t.Fatalf("Error() = %q, want %q", got, want)
		}
	})

	t.Run("non-string value", func(t *testing.T) {
		// A panic value need not be a string; %v must still render it.
		pe := &PanicError{Value: 42}
		got := pe.Error()
		want := "parallel: worker panicked: 42"
		if got != want {
			t.Fatalf("Error() = %q, want %q", got, want)
		}
	})

	t.Run("via fmt formatting", func(t *testing.T) {
		// Exercise the method through the error/fmt machinery the way callers
		// actually reach it, rather than calling Error() directly.
		var err error = &PanicError{Value: stderrors.New("inner")}
		got := fmt.Sprintf("%v", err)
		want := "parallel: worker panicked: inner"
		if got != want {
			t.Fatalf("%%v = %q, want %q", got, want)
		}
	})
}

func TestMapBounded_UnboundedWhenLimitNegative(t *testing.T) {
	spectest.Proves(t, "go/bounded-parallel-work", "concurrency", "unbounded-when-limit-negative")
	// The docs promise `limit <= 0` is unbounded, but only limit==0 is covered
	// elsewhere. A negative limit must take the same no-semaphore path: every
	// item runs concurrently. We gate all workers behind a release channel that
	// only opens once all of them have arrived, so the test deadlocks (and thus
	// fails) if the negative limit were ever treated as bounded.
	const items = 16
	var (
		inflight atomic.Int32
		maxSeen  atomic.Int32
	)
	release := make(chan struct{})

	go func() {
		for inflight.Load() < items {
			time.Sleep(time.Millisecond)
		}
		close(release)
	}()

	work := make([]int, items)
	results, err := MapBounded(context.Background(), work, -1, func(_ context.Context, _ int) (int, error) {
		cur := inflight.Add(1)
		for {
			peak := maxSeen.Load()
			if cur <= peak || maxSeen.CompareAndSwap(peak, cur) {
				break
			}
		}
		<-release
		return 0, nil
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got := maxSeen.Load(); int(got) < items {
		t.Fatalf("limit=-1 should run all items concurrently, max in-flight = %d, want %d", got, items)
	}
	if len(results) != items {
		t.Fatalf("len(results) = %d, want %d", len(results), items)
	}
}

func TestMapBounded_ParentCancelledAfterSuccessDropsResults(t *testing.T) {
	// Covers the post-wg.Wait() fallback: when no worker reports an error but
	// the parent ctx is canceled by the time the group drains, MapBounded must
	// discard the (fully computed) results and return ctx.Err().
	//
	// The single worker cancels the parent itself and returns a non-nil result
	// with a nil error, so firstErr stays nil while ctx.Err() becomes non-nil —
	// deterministically routing the return through `if err := ctx.Err()`.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	results, err := MapBounded(ctx, []int{1}, 1, func(_ context.Context, i int) (int, error) {
		cancel() // parent cancellation, but this worker still completes cleanly
		return i * 100, nil
	})
	if !stderrors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want %v", err, context.Canceled)
	}
	if results != nil {
		t.Errorf("results = %v, want nil when parent is canceled after success", results)
	}
}

func TestEachBounded_RecoversWorkerPanic(t *testing.T) {
	err := EachBounded(context.Background(), []int{1, 2, 3}, 2, func(_ context.Context, i int) error {
		if i == 2 {
			panic("boom")
		}
		return nil
	})
	var pe *PanicError
	if !stderrors.As(err, &pe) {
		t.Fatalf("err = %v, want *PanicError", err)
	}
}

// Example usage — also acts as a smoke test for the documented API.
func ExampleMapBounded() {
	ctx := context.Background()
	items := []int{1, 2, 3, 4}
	results, err := MapBounded(ctx, items, 2, func(_ context.Context, i int) (int, error) {
		return i * i, nil
	})
	fmt.Println(results, err)
	// Output: [1 4 9 16] <nil>
}
