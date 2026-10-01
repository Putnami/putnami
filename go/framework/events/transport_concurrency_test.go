package events

import (
	"sync"
	"testing"
)

// --- Process-wide activeTransport access is data-race free ---

// TestSetTransport_ConcurrentAccess hammers the package-global transport from
// many goroutines: writers flip it between a real transport and nil (mirroring
// Configure/start racing Stop's SetTransport(nil)) while readers call the public
// accessors. It must run clean under `go test -race` / `putnami test --race`.
func TestSetTransport_ConcurrentAccess(t *testing.T) {
	SetTransport(nil)
	defer SetTransport(nil)

	transport := &recordingTransport{}

	const goroutines = 50
	var wg sync.WaitGroup
	for range goroutines {
		wg.Go(func() { SetTransport(transport) })
		wg.Go(func() { SetTransport(nil) })
		wg.Go(func() { _ = GetTransport() })
	}
	wg.Wait()
}
