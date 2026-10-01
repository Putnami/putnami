package cache

import (
	"context"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
)

// --- Observability: Stats ---

func TestMemoryCacheStatsHitsAndMisses(t *testing.T) {
	c := NewMemoryCache(MemoryConfig{})
	defer c.Close()
	ctx := context.Background()

	c.Set(ctx, "k", []byte("v"), 0)

	c.Get(ctx, "k")       // hit
	c.Get(ctx, "k")       // hit
	c.Get(ctx, "missing") // miss

	s := c.Stats()
	if s.Hits != 2 {
		t.Fatalf("Hits = %d, want 2", s.Hits)
	}
	if s.Misses != 1 {
		t.Fatalf("Misses = %d, want 1", s.Misses)
	}
	if s.Entries != 1 {
		t.Fatalf("Entries = %d, want 1", s.Entries)
	}
}

// TestMemoryCacheStatsExpiredCountsAsMiss asserts a lookup of an expired entry
// is recorded as a miss, not a hit.
func TestMemoryCacheStatsExpiredCountsAsMiss(t *testing.T) {
	c := NewMemoryCache(MemoryConfig{CleanupInterval: -1})
	defer c.Close()
	ctx := context.Background()

	c.Set(ctx, "k", []byte("v"), time.Nanosecond)
	time.Sleep(time.Millisecond)

	if _, ok := c.Get(ctx, "k"); ok {
		t.Fatal("expected expired miss")
	}

	s := c.Stats()
	if s.Hits != 0 {
		t.Fatalf("Hits = %d, want 0", s.Hits)
	}
	if s.Misses != 1 {
		t.Fatalf("Misses = %d, want 1", s.Misses)
	}
}

func TestMemoryCacheStatsEvictions(t *testing.T) {
	spectest.Proves(t, "go/cache-lifecycle", "eviction", "memory-capacity-evictions-counted")
	c := NewMemoryCache(MemoryConfig{MaxEntries: 2})
	defer c.Close()
	ctx := context.Background()

	c.Set(ctx, "a", []byte("1"), 0)
	c.Set(ctx, "b", []byte("2"), 0)
	c.Set(ctx, "c", []byte("3"), 0) // evicts "a"
	c.Set(ctx, "d", []byte("4"), 0) // evicts "b"

	s := c.Stats()
	if s.Evictions != 2 {
		t.Fatalf("Evictions = %d, want 2", s.Evictions)
	}
	if s.Entries != 2 {
		t.Fatalf("Entries = %d, want 2", s.Entries)
	}
}

// TestMemoryCacheStatsOverwriteNoEviction asserts overwriting an existing key
// does not record an eviction.
func TestMemoryCacheStatsOverwriteNoEviction(t *testing.T) {
	spectest.Proves(t, "go/cache-lifecycle", "eviction", "overwrite-does-not-count-as-eviction")
	c := NewMemoryCache(MemoryConfig{MaxEntries: 2})
	defer c.Close()
	ctx := context.Background()

	c.Set(ctx, "a", []byte("1"), 0)
	c.Set(ctx, "b", []byte("2"), 0)
	c.Set(ctx, "a", []byte("updated"), 0) // overwrite, no eviction

	if s := c.Stats(); s.Evictions != 0 {
		t.Fatalf("Evictions = %d, want 0 (overwrite must not evict)", s.Evictions)
	}
}
