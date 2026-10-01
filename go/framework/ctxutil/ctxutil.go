// Package ctxutil holds small, dependency-free context helpers shared across
// the Putnami Go framework's request I/O paths.
//
// Its single entry point is WithRequestTimeout, which bounds one
// control-plane / non-streaming operation with a deadline derived from the
// caller's context. Centralizing the "detached context + bounded deadline"
// pattern keeps the timeout invariant locally checkable and gives the audit
// guard one shape to scan for.
package ctxutil

import (
	"context"
	"time"
)

// WithRequestTimeout bounds a control-plane / non-streaming operation with
// timeout, derived from the caller's ctx. A zero or negative timeout leaves ctx
// unchanged (and returns a no-op cancel), so callers can always defer cancel()
// unconditionally.
//
// It is the single source of truth for the framework's per-operation request
// deadline. Two shapes rely on it:
//
//   - A control-plane call whose enclosing request context already carries a
//     deadline, but which wants a tighter per-operation bound.
//   - A detached context — one built with context.WithoutCancel to survive a
//     waiter's cancellation (e.g. a singleflight leader) — which MUST be
//     re-bounded immediately, since detaching also strips the request deadline.
//
// Streaming reads must NOT use this: canceling the derived context on return
// closes the returned body, so a streaming Get is bounded only by the caller's
// context instead.
func WithRequestTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}
