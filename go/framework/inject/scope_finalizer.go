package inject

import (
	"context"
	"sort"

	"go.putnami.dev/errors"
)

// ScopeFinalizer is implemented by a scoped value that must reconcile with the
// outcome of the work that ran in its scope before that scope is closed.
//
// A scope-aware boundary (for example a per-request HTTP scope in
// go.putnami.dev/http) calls DetachedScope.Finalize exactly once when the work
// finishes, passing the outcome: a nil outcome means the work succeeded, a
// non-nil outcome means it failed (an error result, a canceled/timed-out
// context, or a recovered panic). Implementations commit their pending effects
// on success and roll them back on failure. FinalizeScope runs before Close, so
// a transactional value (a database UnitOfWork) can commit or roll back while
// its resources are still live, and it must be safe to call on a canceled ctx —
// the boundary strips cancellation where a rollback still has to run.
type ScopeFinalizer interface {
	FinalizeScope(ctx context.Context, outcome error) error
}

// Finalize runs the ScopeFinalizer hook on every value materialized in this
// scope that implements it, passing outcome (nil = success, non-nil = failure).
// A scope-aware boundary calls it once, before Close, so a request-scoped
// transaction commits on success and rolls back on error/panic. It is safe to
// call on a scope that materialized no finalizer — then it is a no-op — and it
// is independent of Close, which still runs the ordinary close hooks afterward.
func (s *DetachedScope) Finalize(ctx context.Context, outcome error) error {
	return s.container.finalizeScope(ctx, outcome)
}

// finalizeScope invokes FinalizeScope on every instance materialized in this
// container that implements ScopeFinalizer, in a deterministic (token-key
// sorted) order, aggregating any errors. It walks only this container's own
// instance cache — scoped instances live here, never in a parent — so a
// singleton in a parent container is never finalized per scope. It is a no-op
// when nothing materialized implements ScopeFinalizer, so a scope with no
// transactional work pays only a single guarded map scan.
func (c *Container) finalizeScope(ctx context.Context, outcome error) error {
	c.mu.RLock()
	keys := make([]string, 0, len(c.instances))
	for k := range c.instances {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	finalizers := make([]ScopeFinalizer, 0, len(keys))
	for _, k := range keys {
		if f, ok := c.instances[k].(ScopeFinalizer); ok {
			finalizers = append(finalizers, f)
		}
	}
	c.mu.RUnlock()

	var errs []error
	for _, f := range finalizers {
		if err := f.FinalizeScope(ctx, outcome); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.NewAggregate("scope finalize", errs)
}
