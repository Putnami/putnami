# Cache

The `cache` package provides a generic caching layer with an in-memory implementation. All caches implement the `Cache` interface and are safe for concurrent use.

## Cache Interface

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

All values are stored as `[]byte`. Serialize your types before storing (e.g., JSON, protobuf). A zero `ttl` means no expiration.

## In-Memory Cache

Fast, thread-safe cache backed by a Go map with optional size limits and FIFO eviction:

```go
import "go.putnami.dev/cache"

c := cache.NewMemoryCache(cache.MemoryConfig{
    MaxEntries:      1000,          // 0 = unlimited
    CleanupInterval: time.Minute,   // expired entry cleanup
})
defer c.Close()

c.Set(ctx, "user:42", data, 5*time.Minute)

val, ok := c.Get(ctx, "user:42")
```

When `MaxEntries` is reached, the oldest entry is evicted (FIFO).

## Observability

Every concrete cache implements the optional `StatsProvider` interface, exposing a `Stats()` snapshot of hit/miss/eviction counters and the current entry count:

```go
type Stats struct {
    Hits      uint64 // lookups that returned a live value
    Misses    uint64 // lookups that found nothing (absent or expired)
    Evictions uint64 // entries removed to enforce a size limit
    Entries   int    // entries currently held
}

s := c.Stats()
hitRate := float64(s.Hits) / float64(s.Hits+s.Misses)
```

`Stats` is not part of the core `Cache` interface — it is additive, so third-party implementations are unaffected.

## DI Integration

Register the cache in the DI container:

```go
app.ProvideFunc(func() cache.Cache {
    return cache.NewMemoryCache(cache.MemoryConfig{MaxEntries: 5000})
})
```

## Support and contract

The SDD owner is `go`. `go.putnami.dev/cache` is public, documented,
maintained, and classified **stable** in the repository
[support catalog](../../../putnami.support.json). Before v1.0, minor releases
may still require documented migrations; see the repository
[release policy](../../../RELEASE.md).

The durable behavior contract is [`go/cache-lifecycle`](specs/cache-lifecycle.json).
Its expiration and eviction tradeoffs are recorded in
[ADR 0001](doc/adr/0001-expiration-and-capacity-eviction.md) and protected by
[`cache_test.go`](cache_test.go), [`lifecycle_test.go`](lifecycle_test.go), and
[`stats_test.go`](stats_test.go).
