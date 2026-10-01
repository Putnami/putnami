// Package cache provides a generic caching layer with an in-memory
// implementation. All caches are safe for concurrent use.
package cache

import (
	"context"
	"time"
)

// Cache is the interface for all cache implementations.
//
// Value ownership: implementations do not alias the caller's backing array in
// either direction. Set copies the value, so the caller may reuse or mutate the
// slice after Set returns; Get returns a fresh copy that the caller owns and may
// mutate freely without affecting cached state or racing other readers.
//
// Context cancellation: in-memory backends complete without blocking and do
// not consult ctx.
type Cache interface {
	// Get retrieves a value by key. Returns the value and true if found,
	// or the zero value and false if not found or expired. The returned slice
	// is a copy owned by the caller.
	Get(ctx context.Context, key string) ([]byte, bool)

	// Set stores a value with an optional TTL. A zero TTL means no expiration.
	// The value is copied; the caller may mutate the slice after Set returns.
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error

	// Delete removes a value by key.
	Delete(ctx context.Context, key string) error

	// Has returns true if the key exists and has not expired.
	Has(ctx context.Context, key string) bool

	// Clear removes all entries from the cache.
	Clear(ctx context.Context) error

	// Close releases any resources held by the cache.
	Close() error
}

// Stats is a point-in-time snapshot of cache observability counters.
//
// Hits and Misses count lookups (Get) that did and did not return a live value;
// hit rate is Hits / (Hits + Misses). Evictions counts entries removed to stay
// within a size limit (it does not count Delete or TTL expiry). Entries is the
// number of entries currently held. The counters are monotonic for the lifetime
// of the cache and reset only by recreating it.
type Stats struct {
	Hits      uint64 // lookups that returned a live value
	Misses    uint64 // lookups that found nothing (absent or expired)
	Evictions uint64 // entries removed to enforce a size limit
	Entries   int    // entries currently held
}

// StatsProvider is implemented by caches that expose observability counters.
// MemoryCache satisfies it. It is kept separate from Cache so the core
// interface stays minimal and third-party implementations are not forced to
// provide metrics.
type StatsProvider interface {
	// Stats returns a snapshot of the cache's counters.
	Stats() Stats
}

// entry is an internal cache entry with expiration tracking.
type entry struct {
	Value     []byte
	ExpiresAt time.Time // zero means no expiration
}

func (e entry) isExpired() bool {
	if e.ExpiresAt.IsZero() {
		return false
	}
	return time.Now().After(e.ExpiresAt)
}
