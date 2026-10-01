package cache

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestMemoryCache_ConcurrentSetGet(t *testing.T) {
	c := NewMemoryCache(MemoryConfig{MaxEntries: 1000})
	defer c.Close()
	ctx := context.Background()

	const goroutines = 50
	const opsPerGoroutine = 20

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := range goroutines {
		go func() {
			defer wg.Done()
			for j := range opsPerGoroutine {
				key := fmt.Sprintf("key-%d-%d", i, j)
				val := []byte(fmt.Sprintf("val-%d-%d", i, j))
				if err := c.Set(ctx, key, val, time.Minute); err != nil {
					t.Errorf("Set(%s): %v", key, err)
					return
				}
				got, ok := c.Get(ctx, key)
				if !ok {
					// May be evicted by other goroutines, that's acceptable
					continue
				}
				if string(got) != string(val) {
					t.Errorf("Get(%s) = %q, want %q", key, got, val)
				}
			}
		}()
	}

	wg.Wait()
}

func TestMemoryCache_ConcurrentDelete(t *testing.T) {
	c := NewMemoryCache(MemoryConfig{MaxEntries: 0})
	defer c.Close()
	ctx := context.Background()

	const goroutines = 30

	// Pre-populate
	for i := range goroutines {
		key := fmt.Sprintf("key-%d", i)
		if err := c.Set(ctx, key, []byte("value"), time.Minute); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	wg.Add(goroutines * 2)

	// Half delete, half read concurrently
	for i := range goroutines {
		go func() {
			defer wg.Done()
			key := fmt.Sprintf("key-%d", i)
			_ = c.Delete(ctx, key)
		}()
		go func() {
			defer wg.Done()
			key := fmt.Sprintf("key-%d", i)
			c.Get(ctx, key) // may or may not find it
		}()
	}

	wg.Wait()
}

func TestMemoryCache_ConcurrentHas(t *testing.T) {
	c := NewMemoryCache(MemoryConfig{MaxEntries: 0})
	defer c.Close()
	ctx := context.Background()

	if err := c.Set(ctx, "exists", []byte("yes"), time.Minute); err != nil {
		t.Fatal(err)
	}

	const goroutines = 30
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for range goroutines {
		go func() {
			defer wg.Done()
			if !c.Has(ctx, "exists") {
				t.Error("Has(exists) = false, want true")
			}
			if c.Has(ctx, "missing") {
				t.Error("Has(missing) = true, want false")
			}
		}()
	}

	wg.Wait()
}

// hammerSameKey drives Get/Set/Delete on a single shared key from separate
// goroutines. Run under -race it surfaces unsynchronized access to a key's
// value (the former race where MemoryCache.Get read the entry after
// releasing the lock).
func hammerSameKey(t *testing.T, c Cache) {
	t.Helper()
	ctx := context.Background()

	const iterations = 200
	done := make(chan struct{})

	go func() {
		for i := range iterations {
			_ = c.Set(ctx, "shared", []byte(fmt.Sprintf("v%d", i)), time.Minute)
		}
		done <- struct{}{}
	}()
	go func() {
		for range iterations {
			c.Get(ctx, "shared")
		}
		done <- struct{}{}
	}()
	go func() {
		for range iterations {
			_ = c.Delete(ctx, "shared")
		}
		done <- struct{}{}
	}()

	for range 3 {
		<-done
	}
}

func TestMemoryCache_ConcurrentSameKey(t *testing.T) {
	c := NewMemoryCache(MemoryConfig{})
	defer c.Close()
	hammerSameKey(t, c)
}

func TestMemoryCache_ConcurrentWithEviction(t *testing.T) {
	c := NewMemoryCache(MemoryConfig{MaxEntries: 10})
	defer c.Close()
	ctx := context.Background()

	const goroutines = 30
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := range goroutines {
		go func() {
			defer wg.Done()
			key := fmt.Sprintf("key-%d", i)
			_ = c.Set(ctx, key, []byte("value"), time.Minute)
			c.Get(ctx, key)
			c.Has(ctx, key)
		}()
	}

	wg.Wait()
}
