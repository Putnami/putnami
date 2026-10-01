package errors

import "sync"

// ErrorHook is called when an error is created via New, Newf, Bug, or Bugf.
// Hooks must be goroutine-safe and non-blocking.
type ErrorHook func(err *Error)

type hookEntry struct {
	id   uint64
	hook ErrorHook
}

var (
	hooksMu    sync.RWMutex
	hooks      []hookEntry
	nextHookID uint64
)

// OnError registers a hook that fires on error creation and returns a function
// that deregisters it. Use this to wire telemetry (span recording, metrics)
// without creating import cycles from the errors package.
//
// The returned deregister function is idempotent — calling it more than once
// is safe. Long-lived wiring can ignore it; transient registrations (a plugin
// that is set up and torn down repeatedly, tests) should call it on teardown so
// the hook does not outlive its owner and accumulate across lifecycles.
func OnError(hook ErrorHook) func() {
	hooksMu.Lock()
	nextHookID++
	id := nextHookID
	hooks = append(hooks, hookEntry{id: id, hook: hook})
	hooksMu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			hooksMu.Lock()
			defer hooksMu.Unlock()
			for i := range hooks {
				if hooks[i].id == id {
					hooks = append(hooks[:i], hooks[i+1:]...)
					return
				}
			}
		})
	}
}

// resetHooks clears all registered hooks. Test-only helper kept unexported so
// callers cannot wipe telemetry/observability wiring at runtime.
func resetHooks() {
	hooksMu.Lock()
	defer hooksMu.Unlock()
	hooks = nil
	nextHookID = 0
}

// fireHooks invokes every registered hook for err. Hooks run after the lock is
// released (on a snapshot), so a hook may register more hooks or call resetHooks
// without deadlocking, and each invocation is wrapped in recover so a panicking
// hook cannot abort error creation on the hot path.
func fireHooks(err *Error) {
	hooksMu.RLock()
	snapshot := make([]hookEntry, len(hooks))
	copy(snapshot, hooks)
	hooksMu.RUnlock()
	for _, h := range snapshot {
		func() {
			defer func() { recover() }() //nolint:errcheck // swallow hook panics to protect the error-creation hot path
			h.hook(err)
		}()
	}
}
