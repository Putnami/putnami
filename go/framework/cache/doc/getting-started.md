# Cache

`go.putnami.dev/cache` provides a generic caching layer with an in-memory implementation. All caches are safe for concurrent use and built with the standard library only.

## Installation

```bash
go get go.putnami.dev/cache
```

## The Cache Interface

Every implementation satisfies the `Cache` interface:

```go
type Cache interface {
    Get(ctx context.Context, key string) ([]byte, bool)
    Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
    Delete(ctx context.Context, key string) error
    Has(ctx context.Context, key string) bool
    Clear(ctx context.Context) error
    Close() error
}
```

All values are stored as `[]byte`. Serialize your data before storing and deserialize after retrieval.

## In-Memory Cache

`MemoryCache` keeps entries in a map protected by a read-write mutex. It supports optional entry limits with FIFO eviction and runs a background goroutine to clean up expired entries.

```go
import "go.putnami.dev/cache"

c := cache.NewMemoryCache(cache.MemoryConfig{
    MaxEntries:      1000,          // 0 = unlimited
    CleanupInterval: time.Minute,   // 0 defaults to 1 minute; negative disables cleanup
})
defer c.Close()
```

### Get and Set

```go
ctx := context.Background()

// Store a value with a 5-minute TTL
err := c.Set(ctx, "user:42", []byte(`{"name":"Alice"}`), 5*time.Minute)

// Store a value that never expires
err = c.Set(ctx, "config", []byte("v1"), 0)

// Retrieve a value
val, ok := c.Get(ctx, "user:42")
if ok {
    fmt.Println(string(val)) // {"name":"Alice"}
}
```

### Delete and Clear

```go
// Remove a single entry
err := c.Delete(ctx, "user:42")

// Remove all entries
err = c.Clear(ctx)
```

### Check Existence

```go
if c.Has(ctx, "user:42") {
    // key exists and has not expired
}
```

### TTL and Expiration

Pass a positive `time.Duration` as the TTL to `Set`. A zero TTL means the entry never expires. Expired entries are removed lazily on access and periodically by the background cleanup goroutine.

```go
// Expires after 30 seconds
c.Set(ctx, "session:abc", []byte("token"), 30*time.Second)

// Never expires
c.Set(ctx, "static", []byte("data"), 0)
```

### Entry Limit and Eviction

When `MaxEntries` is set and the cache is full, the oldest entry (by insertion order, FIFO) is evicted to make room. Overwriting an existing key does not count as a new entry and does not trigger eviction.

```go
c := cache.NewMemoryCache(cache.MemoryConfig{MaxEntries: 2})
defer c.Close()

c.Set(ctx, "a", []byte("1"), 0)
c.Set(ctx, "b", []byte("2"), 0)
c.Set(ctx, "c", []byte("3"), 0) // evicts "a"

_, ok := c.Get(ctx, "a") // ok == false
```

## Observing Cache Health

The single most important health signal for a cache is its hit rate. Every concrete cache implements the optional `StatsProvider` interface:

```go
type StatsProvider interface {
    Stats() Stats
}

type Stats struct {
    Hits      uint64 // lookups (Get) that returned a live value
    Misses    uint64 // lookups that found nothing (absent or expired)
    Evictions uint64 // entries removed to enforce a size limit (not Delete/TTL)
    Entries   int    // entries currently held
}
```

`Stats` is deliberately **not** part of the core `Cache` interface — it is additive, so the interface stays minimal and third-party implementations are not forced to provide metrics. Type-assert or accept a `StatsProvider` where you need it.

```go
s := c.Stats()
if total := s.Hits + s.Misses; total > 0 {
    log.Printf("cache hit rate: %.1f%% (%d entries, %d evictions)",
        100*float64(s.Hits)/float64(total), s.Entries, s.Evictions)
}
```

A low hit rate combined with a rising `Evictions` count is the classic signature of an undersized `MaxEntries` (an eviction storm); a low hit rate with no evictions usually means a too-short TTL.

## Closing Caches

Always call `Close()` when a cache is no longer needed. This stops background cleanup goroutines and releases resources. Use `defer` to ensure cleanup:

```go
c := cache.NewMemoryCache(cache.MemoryConfig{})
defer c.Close()
```

## Best Practices

- **Always close caches.** Each cache starts a background goroutine for cleanup. Failing to call `Close()` leaks goroutines.
- **Set a `MaxEntries` limit** in long-running services to prevent unbounded memory growth. Without it, entries written with `ttl=0` accumulate forever.
- **Use TTLs.** Avoid storing entries without expiration unless the data is truly static. TTLs prevent stale data and keep resource usage bounded.
- **Serialize consistently.** The cache stores raw `[]byte`. Use `json.Marshal`/`json.Unmarshal` or another codec to convert your types.
- **Tune cleanup intervals.** The default interval (1 minute) suits most workloads. A lower interval reduces peak memory usage at the cost of more frequent lock acquisition. Set `CleanupInterval` to a negative value to disable the background goroutine entirely (expired entries are then only removed lazily on access).
