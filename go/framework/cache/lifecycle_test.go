package cache

import (
	"context"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
)

// --- Close idempotency ---

func TestMemoryCacheCloseIdempotent(t *testing.T) {
	c := NewMemoryCache(MemoryConfig{})
	if err := c.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := c.Close(); err != nil { // must not panic on "close of closed channel"
		t.Fatalf("second Close: %v", err)
	}
}

// --- Background cleanup ---

func TestMemoryCacheCleanupDisabled(t *testing.T) {
	c := NewMemoryCache(MemoryConfig{CleanupInterval: -1})
	defer c.Close()
	ctx := context.Background()

	c.Set(ctx, "k", []byte("v"), 10*time.Millisecond)
	time.Sleep(50 * time.Millisecond)

	// With cleanup disabled, no background goroutine sweeps the entry, so it
	// is still counted (lazy expiry only happens on access).
	if c.Len() != 1 {
		t.Fatalf("Len() = %d, want 1 (background cleanup disabled)", c.Len())
	}
}

func TestMemoryCacheCleanupRemovesExpired(t *testing.T) {
	c := NewMemoryCache(MemoryConfig{CleanupInterval: 5 * time.Millisecond})
	defer c.Close()
	ctx := context.Background()

	c.Set(ctx, "k", []byte("v"), 10*time.Millisecond)

	deadline := time.Now().Add(2 * time.Second)
	for c.Len() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("background cleanup did not remove expired entry; Len()=%d", c.Len())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestMemoryCacheRemoveExpiredSweep(t *testing.T) {
	spectest.Proves(t, "go/cache-lifecycle", "expiration", "memory-zero-ttl-never-expires")
	c := NewMemoryCache(MemoryConfig{CleanupInterval: -1})
	defer c.Close()
	ctx := context.Background()

	c.Set(ctx, "expired", []byte("v"), time.Nanosecond)
	c.Set(ctx, "live", []byte("v"), 0)
	time.Sleep(time.Millisecond)

	c.removeExpired()

	if c.Len() != 1 {
		t.Fatalf("Len() = %d after sweep, want 1", c.Len())
	}
	if !c.Has(ctx, "live") {
		t.Fatal("non-expired entry should survive the sweep")
	}
}

func TestMemoryCacheStaleExpiredReadDoesNotDeleteFreshValue(t *testing.T) {
	c := NewMemoryCache(MemoryConfig{CleanupInterval: -1})
	defer c.Close()
	ctx := context.Background()

	if err := c.Set(ctx, "k", []byte("old"), time.Nanosecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)

	c.mu.RLock()
	elem := c.entries["k"]
	val := elem.Value.(*keyEntry).value
	c.mu.RUnlock()
	if !val.isExpired() {
		t.Fatal("test setup expected an expired value")
	}

	if err := c.Set(ctx, "k", []byte("fresh"), time.Minute); err != nil {
		t.Fatal(err)
	}

	c.mu.Lock()
	c.removeElementIfExpired("k", elem)
	c.mu.Unlock()

	got, ok := c.Get(ctx, "k")
	if !ok {
		t.Fatal("fresh value was removed by stale expired read")
	}
	if string(got) != "fresh" {
		t.Fatalf("Get(k) = %q, want %q", got, "fresh")
	}
}
