package cache

import (
	"context"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
)

// --- MemoryCache ---

func TestMemoryCacheGetSet(t *testing.T) {
	c := NewMemoryCache(MemoryConfig{})
	defer c.Close()
	ctx := context.Background()

	c.Set(ctx, "key1", []byte("value1"), 0)

	val, ok := c.Get(ctx, "key1")
	if !ok || string(val) != "value1" {
		t.Fatalf("Get(key1) = %q, %v; want %q, true", val, ok, "value1")
	}
}

// TestMemoryCacheSetCopiesInput asserts the cache copies the value on Set, so a
// caller mutating the input slice afterwards cannot corrupt cached state.
func TestMemoryCacheSetCopiesInput(t *testing.T) {
	spectest.Proves(t, "go/cache-lifecycle", "ownership", "set-copies-caller-slice")
	c := NewMemoryCache(MemoryConfig{})
	defer c.Close()
	ctx := context.Background()

	input := []byte("value1")
	c.Set(ctx, "key1", input, 0)
	for i := range input {
		input[i] = 'X' // mutate the caller's slice after Set
	}

	val, ok := c.Get(ctx, "key1")
	if !ok || string(val) != "value1" {
		t.Fatalf("Get(key1) = %q, %v; want %q, true (mutating input must not affect cache)", val, ok, "value1")
	}
}

// TestMemoryCacheGetReturnsCopy asserts Get returns a copy the caller owns, so
// mutating it cannot corrupt cached state or race other readers.
func TestMemoryCacheGetReturnsCopy(t *testing.T) {
	spectest.Proves(t, "go/cache-lifecycle", "ownership", "get-returns-caller-owned-copy")
	c := NewMemoryCache(MemoryConfig{})
	defer c.Close()
	ctx := context.Background()

	c.Set(ctx, "key1", []byte("value1"), 0)

	first, ok := c.Get(ctx, "key1")
	if !ok {
		t.Fatal("expected hit")
	}
	for i := range first {
		first[i] = 'X' // mutate the returned slice
	}

	second, ok := c.Get(ctx, "key1")
	if !ok || string(second) != "value1" {
		t.Fatalf("Get(key1) = %q, %v; want %q, true (mutating a returned slice must not affect cache)", second, ok, "value1")
	}
}

func TestMemoryCacheMiss(t *testing.T) {
	c := NewMemoryCache(MemoryConfig{})
	defer c.Close()

	_, ok := c.Get(context.Background(), "missing")
	if ok {
		t.Fatal("expected cache miss")
	}
}

func TestMemoryCacheHas(t *testing.T) {
	c := NewMemoryCache(MemoryConfig{})
	defer c.Close()
	ctx := context.Background()

	if c.Has(ctx, "key1") {
		t.Fatal("Has(key1) should be false before Set")
	}

	c.Set(ctx, "key1", []byte("v"), 0)

	if !c.Has(ctx, "key1") {
		t.Fatal("Has(key1) should be true after Set")
	}
}

func TestMemoryCacheDelete(t *testing.T) {
	c := NewMemoryCache(MemoryConfig{})
	defer c.Close()
	ctx := context.Background()

	c.Set(ctx, "key1", []byte("v"), 0)
	c.Delete(ctx, "key1")

	_, ok := c.Get(ctx, "key1")
	if ok {
		t.Fatal("expected cache miss after Delete")
	}
}

func TestMemoryCacheClear(t *testing.T) {
	c := NewMemoryCache(MemoryConfig{})
	defer c.Close()
	ctx := context.Background()

	c.Set(ctx, "a", []byte("1"), 0)
	c.Set(ctx, "b", []byte("2"), 0)
	c.Clear(ctx)

	if c.Len() != 0 {
		t.Fatalf("Len() = %d after Clear, want 0", c.Len())
	}
}

func TestMemoryCacheTTL(t *testing.T) {
	spectest.Proves(t, "go/cache-lifecycle", "expiration", "memory-positive-ttl-expires")
	c := NewMemoryCache(MemoryConfig{})
	defer c.Close()
	ctx := context.Background()

	c.Set(ctx, "key1", []byte("value"), 50*time.Millisecond)

	// Should be available immediately
	val, ok := c.Get(ctx, "key1")
	if !ok || string(val) != "value" {
		t.Fatal("expected value before expiry")
	}

	time.Sleep(60 * time.Millisecond)

	// Should be expired
	_, ok = c.Get(ctx, "key1")
	if ok {
		t.Fatal("expected cache miss after TTL expiry")
	}
}

func TestMemoryCacheMaxEntries(t *testing.T) {
	spectest.Proves(t, "go/cache-lifecycle", "eviction", "memory-max-entries-bounds-cache")
	c := NewMemoryCache(MemoryConfig{MaxEntries: 2})
	defer c.Close()
	ctx := context.Background()

	c.Set(ctx, "a", []byte("1"), 0)
	c.Set(ctx, "b", []byte("2"), 0)
	c.Set(ctx, "c", []byte("3"), 0) // should evict "a" (FIFO)

	_, ok := c.Get(ctx, "a")
	if ok {
		t.Fatal("expected 'a' to be evicted")
	}

	val, ok := c.Get(ctx, "c")
	if !ok || string(val) != "3" {
		t.Fatal("expected 'c' to be present")
	}

	if c.Len() != 2 {
		t.Fatalf("Len() = %d, want 2", c.Len())
	}
}

func TestMemoryCacheOverwriteDoesNotEvict(t *testing.T) {
	spectest.Proves(t, "go/cache-lifecycle", "eviction", "overwrite-does-not-evict")
	c := NewMemoryCache(MemoryConfig{MaxEntries: 2})
	defer c.Close()
	ctx := context.Background()

	c.Set(ctx, "a", []byte("1"), 0)
	c.Set(ctx, "b", []byte("2"), 0)
	c.Set(ctx, "a", []byte("updated"), 0) // overwrite, should NOT evict

	val, ok := c.Get(ctx, "a")
	if !ok || string(val) != "updated" {
		t.Fatalf("expected updated value, got %q, %v", val, ok)
	}

	if c.Len() != 2 {
		t.Fatalf("Len() = %d, want 2", c.Len())
	}
}
