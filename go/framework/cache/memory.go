package cache

import (
	"container/list"
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// MemoryConfig configures the in-memory cache.
type MemoryConfig struct {
	// MaxEntries is the maximum number of entries. 0 means unlimited.
	MaxEntries int

	// CleanupInterval is the interval for removing expired entries.
	// Defaults to 1 minute when zero. A negative value disables the
	// background cleanup goroutine entirely (expired entries are then only
	// removed lazily on access).
	CleanupInterval time.Duration
}

// MemoryCache is a thread-safe in-memory cache with TTL and optional size limits.
// When MaxEntries is exceeded, the oldest entry is evicted (FIFO).
type MemoryCache struct {
	entries map[string]*list.Element // key → list element for O(1) lookup and removal
	order   *list.List               // insertion order for FIFO eviction
	cfg     MemoryConfig
	mu      sync.RWMutex
	done    chan struct{}
	closeMu sync.Once // guards done so Close is idempotent

	// Observability counters. Incremented atomically so a reader does not need
	// c.mu; they are independent of the entry map's lock.
	hits      atomic.Uint64
	misses    atomic.Uint64
	evictions atomic.Uint64
}

// keyEntry pairs a key with its cached value inside the linked list.
type keyEntry struct {
	key   string
	value entry
}

// NewMemoryCache creates a new in-memory cache.
func NewMemoryCache(cfg MemoryConfig) *MemoryCache {
	c := &MemoryCache{
		entries: make(map[string]*list.Element),
		order:   list.New(),
		cfg:     cfg,
		done:    make(chan struct{}),
	}

	// A negative interval disables background cleanup; zero uses the default.
	if interval := cfg.CleanupInterval; interval >= 0 {
		if interval == 0 {
			interval = time.Minute
		}
		go c.cleanup(interval)
	}

	return c
}

// Get retrieves a value from the memory cache.
func (c *MemoryCache) Get(_ context.Context, key string) ([]byte, bool) {
	c.mu.RLock()
	elem, ok := c.entries[key]
	if !ok {
		c.mu.RUnlock()
		c.misses.Add(1)
		return nil, false
	}
	// Copy the entry header while holding the read lock so we never touch shared
	// state after releasing it (Set overwrites *keyEntry.value under the write
	// lock). Set replaces the slice rather than mutating it, so the backing
	// bytes are stable once captured.
	val := elem.Value.(*keyEntry).value //nolint:errcheck // list elements are always *keyEntry
	c.mu.RUnlock()

	if val.isExpired() {
		c.mu.Lock()
		c.removeElementIfExpired(key, elem)
		c.mu.Unlock()
		c.misses.Add(1)
		return nil, false
	}

	// Return a copy so the caller owns its bytes and cannot mutate cached state
	// (or race other readers) through the returned slice.
	c.hits.Add(1)
	return cloneBytes(val.Value), true
}

// cloneBytes returns a copy of b, preserving nil. The cache uses it to avoid
// aliasing the caller's backing array on both store and return.
func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	c := make([]byte, len(b))
	copy(c, b)
	return c
}

// Set stores a value in the memory cache.
func (c *MemoryCache) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Evict if at capacity
	if c.cfg.MaxEntries > 0 && len(c.entries) >= c.cfg.MaxEntries {
		if _, exists := c.entries[key]; !exists {
			c.evictOldest()
		}
	}

	var expiresAt time.Time
	if ttl > 0 {
		expiresAt = time.Now().Add(ttl)
	}

	// Store a copy so the cache owns its bytes: a caller mutating the input
	// slice after Set must not corrupt cached state or race concurrent readers.
	e := entry{
		Value:     cloneBytes(value),
		ExpiresAt: expiresAt,
	}

	if elem, exists := c.entries[key]; exists {
		// Update existing entry in place
		elem.Value.(*keyEntry).value = e //nolint:errcheck // list elements are always *keyEntry
	} else {
		// Add new entry at the back (newest)
		ke := &keyEntry{key: key, value: e}
		c.entries[key] = c.order.PushBack(ke)
	}

	return nil
}

// Delete removes a value from the memory cache.
func (c *MemoryCache) Delete(_ context.Context, key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.removeElement(key)
	return nil
}

// Has reports whether a non-expired entry exists in the memory cache.
func (c *MemoryCache) Has(_ context.Context, key string) bool {
	c.mu.RLock()
	elem, ok := c.entries[key]
	if !ok {
		c.mu.RUnlock()
		return false
	}
	val := elem.Value.(*keyEntry).value //nolint:errcheck // list elements are always *keyEntry
	c.mu.RUnlock()

	if val.isExpired() {
		c.mu.Lock()
		c.removeElementIfExpired(key, elem)
		c.mu.Unlock()
		return false
	}

	return true
}

// Clear removes all entries from the memory cache.
func (c *MemoryCache) Clear(_ context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.entries = make(map[string]*list.Element)
	c.order.Init()
	return nil
}

// Close stops background cleanup and releases resources. It is idempotent;
// calling it more than once is safe and does not panic.
func (c *MemoryCache) Close() error {
	c.closeMu.Do(func() {
		close(c.done)
	})
	return nil
}

// Len returns the number of entries (including potentially expired ones).
func (c *MemoryCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}

// Stats returns a snapshot of the cache's hit/miss/eviction counters and the
// current entry count. It satisfies StatsProvider. Entries counts entries still
// present, including any that have expired but not yet been swept.
func (c *MemoryCache) Stats() Stats {
	return Stats{
		Hits:      c.hits.Load(),
		Misses:    c.misses.Load(),
		Evictions: c.evictions.Load(),
		Entries:   c.Len(),
	}
}

func (c *MemoryCache) evictOldest() {
	front := c.order.Front()
	if front == nil {
		return
	}
	ke := front.Value.(*keyEntry) //nolint:errcheck // list elements are always *keyEntry
	c.order.Remove(front)
	delete(c.entries, ke.key)
	c.evictions.Add(1)
}

// removeElement removes a key and its list element in O(1).
func (c *MemoryCache) removeElement(key string) {
	elem, ok := c.entries[key]
	if !ok {
		return
	}
	c.order.Remove(elem)
	delete(c.entries, key)
}

// removeElementIfExpired removes key only if it still points at elem and the
// current value is expired. The caller must hold c.mu.
func (c *MemoryCache) removeElementIfExpired(key string, elem *list.Element) {
	current, ok := c.entries[key]
	if !ok || current != elem {
		return
	}
	ke := current.Value.(*keyEntry) //nolint:errcheck // list elements are always *keyEntry
	if !ke.value.isExpired() {
		return
	}
	c.removeElement(key)
}

func (c *MemoryCache) cleanup(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
			c.removeExpired()
		}
	}
}

// removeExpired sweeps the cache and removes all expired entries. It is the
// body of a single cleanup tick, exposed separately so it can be exercised
// deterministically by tests.
func (c *MemoryCache) removeExpired() {
	c.mu.Lock()
	defer c.mu.Unlock()

	var expired []string
	for key, elem := range c.entries {
		ke := elem.Value.(*keyEntry) //nolint:errcheck // list elements are always *keyEntry
		if ke.value.isExpired() {
			expired = append(expired, key)
		}
	}
	for _, key := range expired {
		c.removeElement(key)
	}
}
