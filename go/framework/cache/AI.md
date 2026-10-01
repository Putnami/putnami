# go.putnami.dev/cache

An in-memory cache with TTL and FIFO eviction. All caches implement the
`Cache` interface and are safe for concurrent use. Values are stored as
`[]byte`; serialize before `Set` and deserialize after `Get`.

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

A zero `ttl` means the entry never expires.

## Observability

`MemoryCache` implements the optional `StatsProvider` interface —
`Stats() Stats` — returning hit/miss/eviction counters plus the current entry
count. `Stats` is additive and not part of the core `Cache` interface, so
third-party implementations are unaffected.

```go
s := c.Stats() // Hits, Misses, Evictions, Entries
hitRate := float64(s.Hits) / float64(s.Hits+s.Misses)
```

## Quick Start

```go
import "go.putnami.dev/cache"

c := cache.NewMemoryCache(cache.MemoryConfig{
    MaxEntries:      1000,        // 0 = unlimited
    CleanupInterval: time.Minute, // 0 = default; negative disables cleanup
})
defer c.Close()

c.Set(ctx, "key", value, 10*time.Minute) // value is []byte

val, ok := c.Get(ctx, "key")
c.Delete(ctx, "key")
c.Has(ctx, "key")
```

## Backends

```go
// Memory (FIFO eviction when MaxEntries is exceeded)
c := cache.NewMemoryCache(cache.MemoryConfig{MaxEntries: 1000})
```

See `doc/getting-started.md` and `README.md` for the full reference.

## Contract invariants

- Entries are never returned after their expiration deadline.
- Capacity eviction is distinct from deletion and expiration, and byte slices
  never alias caller-owned storage.

Support is **stable** in `../../../putnami.support.json`; the normative feature
contract is `specs/cache-lifecycle.json`, with the decision in
`doc/adr/0001-expiration-and-capacity-eviction.md`.
