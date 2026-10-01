# Caching

`go.putnami.dev/cache` provides a generic caching layer with an in-memory implementation. All caches are safe for concurrent use.

It is a **stable** public package owned by the Go SDD surface. Built-in memory
entries are not returned after expiration. See the cache-lifecycle
specification and accepted expiration and capacity-eviction decision record
next to the package source.

## Cache interface

All implementations share the same interface:

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

## Memory cache

In-memory cache with optional FIFO eviction:

```go
import "go.putnami.dev/cache"

c := cache.NewMemoryCache(cache.MemoryConfig{
    MaxEntries:      1000,           // 0 = unlimited
    CleanupInterval: 5 * time.Minute,
})
defer c.Close()
```

### Configuration

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `MaxEntries` | `int` | `0` | Maximum entries (0 = unlimited). FIFO eviction when exceeded |
| `CleanupInterval` | `time.Duration` | — | Interval for removing expired entries |

## Operations

### Set and get

```go
// Store with TTL
err := c.Set(ctx, "user:123", []byte(`{"name":"Jane"}`), 5*time.Minute)

// Store without expiration
err := c.Set(ctx, "config", data, 0)

// Retrieve
value, found := c.Get(ctx, "user:123")
if !found {
    // cache miss or expired
}
```

### Check and delete

```go
if c.Has(ctx, "user:123") {
    // key exists and is not expired
}

err := c.Delete(ctx, "user:123")

// Clear all entries
err := c.Clear(ctx)
```

## Patterns

### Cache-aside

```go
func (s *UserService) GetUser(ctx context.Context, id string) (*User, error) {
    key := "user:" + id

    // Check cache
    if data, found := s.cache.Get(ctx, key); found {
        var user User
        json.Unmarshal(data, &user)
        return &user, nil
    }

    // Cache miss — fetch from database
    user, err := s.repo.FindByID(ctx, "id", id)
    if err != nil {
        return nil, err
    }

    // Store in cache
    data, _ := json.Marshal(user)
    s.cache.Set(ctx, key, data, 10*time.Minute)

    return &user, nil
}
```

### Cache invalidation

```go
func (s *UserService) UpdateUser(ctx context.Context, id string, input UpdateInput) error {
    if err := s.repo.Update(ctx, id, input); err != nil {
        return err
    }
    // Invalidate cached entry
    return s.cache.Delete(ctx, "user:"+id)
}
```

## Related guides

- [Persistence](/docs/frameworks/go/persistence) — database queries to cache
- [Configuration](/docs/frameworks/go/configuration) — cache configuration
