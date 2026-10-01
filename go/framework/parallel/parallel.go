// Package parallel provides bounded fan-out helpers for running work
// concurrently with context cancellation and first-error-wins semantics.
//
// The two entry points are MapBounded — for work that returns a result —
// and EachBounded — for side-effect-only work. A positive limit caps the
// number of in-flight workers and stops bounded dispatch when cancellation
// is observed. A non-positive limit may create one goroutine per item before
// cancellation is observed; every goroutine checks the context immediately
// before invoking user work.
//
// A panic in a worker is recovered and returned as a *PanicError rather
// than crashing the process, so one bad item cannot escape the caller's
// recovery and take down the program.
package parallel

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"
)

// PanicError is returned by MapBounded and EachBounded when a worker function
// panics. The panic is recovered inside the worker goroutine — so it cannot
// crash the process or escape the caller's recovery — and surfaced as an
// error that participates in the normal first-error-wins and sibling
// cancellation flow.
type PanicError struct {
	// Value is the value passed to panic.
	Value any
	// Stack is the worker goroutine stack captured at recovery time.
	Stack []byte
}

func (e *PanicError) Error() string {
	return fmt.Sprintf("parallel: worker panicked: %v", e.Value)
}

// Unwrap exposes the panic value when it is itself an error so callers can
// match it with errors.Is and errors.As.
func (e *PanicError) Unwrap() error {
	if err, ok := e.Value.(error); ok {
		return err
	}
	return nil
}

// MapBounded runs fn for each item with at most limit goroutines in flight.
//
// Results are returned in input order. On the first error returned by fn,
// MapBounded cancels the context passed to every other worker and returns
// that error. With a positive limit, bounded dispatch stops when it observes
// cancellation. With a non-positive limit, goroutines may already have been
// created for remaining items; each skips fn when cancellation is visible at
// its pre-invocation check. Workers already inside fn receive the canceled
// context and are expected to return promptly.
//
// If fn panics, the panic is recovered in the worker goroutine and returned
// as a *PanicError; it counts as that worker's error — the first error or
// panic wins and siblings are canceled — instead of crashing the process.
//
// limit <= 0 means unbounded — one goroutine per item, equivalent to a
// plain WaitGroup fan-out but with cancellation and first-error semantics.
//
// If items is empty, MapBounded returns (nil, nil) without invoking fn.
// If ctx is already canceled, MapBounded returns (nil, ctx.Err()) without
// invoking fn.
func MapBounded[T, R any](
	ctx context.Context,
	items []T,
	limit int,
	fn func(ctx context.Context, item T) (R, error),
) ([]R, error) {
	if len(items) == 0 {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	results := make([]R, len(items))
	groupCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg       sync.WaitGroup
		firstErr atomic.Pointer[error]
		sem      chan struct{}
	)
	if limit > 0 {
		sem = make(chan struct{}, limit)
	}

	setErr := func(err error) {
		e := err
		if firstErr.CompareAndSwap(nil, &e) {
			cancel()
		}
	}

dispatch:
	for i, item := range items {
		if sem != nil {
			select {
			case sem <- struct{}{}:
			case <-groupCtx.Done():
				break dispatch
			}
			// Both select cases can become ready together when a worker fails
			// while releasing its slot. Recheck cancellation before launching so
			// bounded dispatch stops once that cancellation is observable.
			if groupCtx.Err() != nil {
				<-sem
				break dispatch
			}
		}
		wg.Add(1)
		go func(idx int, it T) {
			defer wg.Done()
			if sem != nil {
				defer func() { <-sem }()
			}
			r, invoked, err := callWorkerIfActive(groupCtx, it, fn)
			if !invoked {
				return
			}
			if err != nil {
				setErr(err)
				return
			}
			results[idx] = r
		}(i, item)
	}
	wg.Wait()

	if p := firstErr.Load(); p != nil {
		return nil, *p
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

// EachBounded runs fn for each item with at most limit goroutines in flight
// for side-effect-only work. It has the same cancellation, first-error, and
// panic-recovery semantics as MapBounded but discards per-item results.
//
// limit <= 0 means unbounded. If items is empty, EachBounded returns nil.
// If ctx is already canceled, EachBounded returns ctx.Err().
func EachBounded[T any](
	ctx context.Context,
	items []T,
	limit int,
	fn func(ctx context.Context, item T) error,
) error {
	_, err := MapBounded(ctx, items, limit, func(c context.Context, it T) (struct{}, error) {
		return struct{}{}, fn(c, it)
	})
	return err
}

// callWorkerIfActive protects the boundary between creating a goroutine and
// invoking caller-owned work. A cancellation racing this check can still enter
// fn, so fn must also honor its context once invoked.
func callWorkerIfActive[T, R any](
	ctx context.Context,
	item T,
	fn func(ctx context.Context, item T) (R, error),
) (r R, invoked bool, err error) {
	select {
	case <-ctx.Done():
		return r, false, nil
	default:
	}
	r, err = callWithRecover(ctx, item, fn)
	return r, true, err
}

// callWithRecover invokes fn and converts a panic into a *PanicError so a
// panicking worker cannot terminate the process. The recovered value and the
// stack at the point of recovery are preserved on the returned error.
func callWithRecover[T, R any](
	ctx context.Context,
	item T,
	fn func(ctx context.Context, item T) (R, error),
) (r R, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = &PanicError{Value: p, Stack: debug.Stack()}
		}
	}()
	return fn(ctx, item)
}
