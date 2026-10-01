package cache_test

import (
	"context"
	"fmt"
	"time"

	"go.putnami.dev/cache"
)

// ExampleMemoryCache mirrors the Quick Start in AI.md / README.md so the
// documented API stays compile-checked and cannot silently drift.
func ExampleMemoryCache() {
	ctx := context.Background()

	c := cache.NewMemoryCache(cache.MemoryConfig{
		MaxEntries:      1000,
		CleanupInterval: time.Minute,
	})
	defer c.Close()

	_ = c.Set(ctx, "key", []byte("value"), 10*time.Minute)

	val, ok := c.Get(ctx, "key")
	fmt.Println(string(val), ok)
	// Output: value true
}
