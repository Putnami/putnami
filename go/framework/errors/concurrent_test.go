package errors

import (
	"sync"
	"testing"
)

// --- Concurrent hook registration and invocation ---

func TestOnError_ConcurrentRegistration(t *testing.T) {
	resetHooks()
	defer resetHooks()

	const goroutines = 50
	var wg sync.WaitGroup

	for range goroutines {
		wg.Go(func() {
			OnError(func(_ *Error) {})
		})
	}
	wg.Wait()

	// Verify all hooks were registered (no lost writes).
	hooksMu.RLock()
	count := len(hooks)
	hooksMu.RUnlock()
	if count != goroutines {
		t.Errorf("expected %d hooks, got %d", goroutines, count)
	}
}

func TestFireHooks_ConcurrentWithRegistration(t *testing.T) {
	resetHooks()
	defer resetHooks()

	// Pre-register some hooks.
	for range 10 {
		OnError(func(_ *Error) {})
	}

	var wg sync.WaitGroup
	// Concurrent writers: register more hooks.
	for range 50 {
		wg.Go(func() {
			OnError(func(_ *Error) {})
		})
	}
	// Concurrent readers: fire hooks via error creation.
	for range 100 {
		wg.Go(func() {
			// New() calls fireHooks internally.
			_ = New(CodeInternal, "test")
		})
	}
	wg.Wait()
}

func TestResetHooks_ConcurrentWithFire(t *testing.T) {
	resetHooks()
	defer resetHooks()

	for range 10 {
		OnError(func(_ *Error) {})
	}

	var wg sync.WaitGroup
	// Concurrent reset.
	for range 20 {
		wg.Go(func() {
			resetHooks()
		})
	}
	// Concurrent fire.
	for range 20 {
		wg.Go(func() {
			_ = New(CodeInternal, "test")
		})
	}
	wg.Wait()
}

// --- Concurrent HTTP status map access ---

func TestRegisterHTTPStatus_ConcurrentRegistration(t *testing.T) {
	// Register custom codes concurrently using high status codes to avoid
	// polluting the default mappings (e.g., 404 → CodeNotFound).
	const goroutines = 50
	var wg sync.WaitGroup

	for i := range goroutines {
		wg.Go(func() {
			code := Code("concurrent.reg." + string(rune('a'+i)))
			RegisterHTTPStatus(code, 600+i)
		})
	}
	wg.Wait()
}

func TestHTTPStatus_ConcurrentReadWrite(t *testing.T) {
	const customCode Code = "concurrent.rw_test"
	RegisterHTTPStatus(customCode, 418)

	var wg sync.WaitGroup
	// Concurrent readers.
	for range 50 {
		wg.Go(func() {
			status := HTTPStatus(New(customCode, "teapot"))
			if status != 418 {
				t.Errorf("expected 418, got %d", status)
			}
		})
	}
	// Concurrent writers (same value to keep reads consistent).
	for range 20 {
		wg.Go(func() {
			RegisterHTTPStatus(customCode, 418)
		})
	}
	wg.Wait()
}

func TestFromStatus_ConcurrentWithRegister(t *testing.T) {
	var wg sync.WaitGroup
	// Concurrent FromStatus lookups (reads the map).
	// Use status 401 → CodeUnauthorized which is unique in the default map.
	for range 50 {
		wg.Go(func() {
			err := FromStatus(401, "unauthorized")
			if err.Code() != CodeUnauthorized {
				t.Errorf("expected %q, got %q", CodeUnauthorized, err.Code())
			}
		})
	}
	// Concurrent registrations with high status codes that don't conflict.
	for i := range 20 {
		wg.Go(func() {
			code := Code("concurrent.from." + string(rune('a'+i)))
			RegisterHTTPStatus(code, 700+i)
		})
	}
	wg.Wait()
}
