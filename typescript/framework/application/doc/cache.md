# Cache System

Putnami provides a powerful layered caching system for server-side data caching. The cache system supports multiple storage backends and is automatically isolated by build version to ensure cache invalidation on deployments.

## Overview

The default cache configuration uses a two-layer architecture:

1. **Memory Layer (L1)** - Fast, volatile in-memory storage
2. **Disk Layer (L2)** - Slower but persistent file-based storage

When reading, the cache checks L1 first. On a miss, it checks L2 and automatically populates L1 for faster subsequent reads. Writes go to both layers (write-through).

## Quick Start

```typescript
import { cache, evictCache, clearCache } from '@putnami/application';

// Cache expensive computation
const result = await cache('compute')
  .ttl(60000)           // 1 minute TTL
  .for('expensive-key') // Cache key
  .fetch(() => computeExpensiveValue());

// Evict specific entries
await evictCache('compute:expensive-key');

// Evict by pattern
await evictCache('compute:*');

// Clear everything
await clearCache();
```

## Fluent Cache API

### `cache(name: string)`

Creates a cache builder with the given namespace.

```typescript
import { cache } from '@putnami/application';

const builder = cache('users');
```

### `.ttl(milliseconds: number)`

Sets the time-to-live for cached entries.

```typescript
cache('users')
  .ttl(300000) // 5 minutes
```

### `.for(key: string | ((...args) => string))`

Sets the cache key. Can be a static string or a function that generates a key from the `fetch()` arguments.

```typescript
// Static key
cache('config').for('app-settings')

// Dynamic key from arguments
cache('users').for((id: string) => `user:${id}`)
```

### `.fetch<T>(fn: () => T | Promise<T>, ...args): Promise<T>`

Executes the function and caches the result. On subsequent calls with the same key, returns the cached value.

```typescript
// Without arguments
const config = await cache('config')
  .ttl(3600000)
  .for('app')
  .fetch(() => loadConfig());

// With arguments (passed to both key function and fetch function)
const user = await cache('users')
  .ttl(60000)
  .for((id: string) => id)
  .fetch(async (id: string) => {
    return await db.users.findById(id);
  }, userId);
```

## Cache Eviction

### `evictCache(pattern: string): Promise<number>`

Evicts all cache entries matching a glob pattern. Returns the number of evicted entries.

```typescript
import { evictCache } from '@putnami/application';

// Evict specific key
await evictCache('users:123');

// Evict all users
await evictCache('users:*');

// Evict everything in a namespace
await evictCache('loader:*');
```

### `clearCache(): Promise<void>`

Clears all cache entries across all layers.

```typescript
import { clearCache } from '@putnami/application';

await clearCache();
```

## Build Version Isolation

Cache directories are automatically namespaced by the build content hash:

```
.putnami/cache/{project}/{contentHash}/{cacheName}/
```

This ensures that:

- Deployments automatically get a fresh cache
- Rolling back to a previous version uses that version's cache
- Multiple versions can run simultaneously without cache conflicts

Old cache versions are automatically evicted (keeping the last 5 versions).

## Storage Implementations

### InMemoryCacheStorage

Fast, volatile storage. Best for frequently accessed data that can be recomputed.

```typescript
import { InMemoryCacheStorage, setCacheStorage } from '@putnami/application';

setCacheStorage(new InMemoryCacheStorage());
```

### DiskCacheStorage

Persistent file-based storage. Survives restarts but slower than memory.

```typescript
import { DiskCacheStorage, setCacheStorage } from '@putnami/application';

setCacheStorage(new DiskCacheStorage('app'));
```

**Constructor options:**

- `cacheName` - Subdirectory name for this cache
- `buildVersion` - Optional explicit version (auto-detected from version.json)
- `customCacheDir` - Optional custom directory (for testing)

### LayeredCacheStorage

Combines multiple storage backends. This is the default configuration.

```typescript
import {
  LayeredCacheStorage,
  InMemoryCacheStorage,
  DiskCacheStorage,
  setCacheStorage
} from '@putnami/application';

setCacheStorage(new LayeredCacheStorage([
  new InMemoryCacheStorage(),  // L1: fast, volatile
  new DiskCacheStorage('app'), // L2: slower, persistent
]));
```

**Behavior:**

- **Reads**: Check layers in order, populate faster layers on hit
- **Writes**: Write to all layers (write-through)
- **Deletes**: Delete from all layers

## Custom Storage

Implement the `CacheStorage` interface for custom backends (Redis, Memcached, etc.):

```typescript
import type { CacheStorage, CacheEntry } from '@putnami/application';

class RedisCacheStorage implements CacheStorage {
  async get<T>(key: string): Promise<CacheEntry<T> | undefined> {
    const data = await redis.get(key);
    if (!data) return undefined;
    return JSON.parse(data);
  }

  async set<T>(key: string, value: T, ttl?: number): Promise<void> {
    const entry = { value, expiresAt: ttl ? Date.now() + ttl : null };
    if (ttl) {
      await redis.setex(key, Math.ceil(ttl / 1000), JSON.stringify(entry));
    } else {
      await redis.set(key, JSON.stringify(entry));
    }
  }

  async delete(key: string): Promise<void> {
    await redis.del(key);
  }

  async deletePattern(pattern: string): Promise<number> {
    const keys = await redis.keys(pattern.replace('*', '*'));
    if (keys.length === 0) return 0;
    return await redis.del(...keys);
  }

  async clear(): Promise<void> {
    await redis.flushdb();
  }
}
```

## Integration with React SSR

The cache system integrates seamlessly with Putnami React loaders and pages:

```typescript
// src/app/posts/loader.ts
import { loader } from '@putnami/web';

export default loader()
  .cache({
    ttl: 60 * 60 * 1000,  // Server-side: 1 hour
    maxAge: 300,           // Browser: 5 minutes
    etag: true             // Enable conditional requests
  })
  .handle(async (ctx) => {
    return { posts: await fetchPosts() };
  });
```

Actions can evict cache entries after mutations:

```typescript
// src/app/posts/action.ts
import { action } from '@putnami/web';

export default action()
  .evict('loader:/posts/*')
  .handle(async (ctx) => {
    await createPost(ctx);
    return { ok: true };
  });
```

## Best Practices

1. **Choose appropriate TTLs** - Balance freshness vs. performance
2. **Use meaningful cache names** - Makes debugging and eviction easier
3. **Prefer pattern eviction** - More maintainable than tracking individual keys
4. **Combine with HTTP caching** - Use `ttl` for server-side, `maxAge` for browser
5. **Monitor cache hit rates** - Tune TTLs based on actual usage patterns

## Troubleshooting

### Cache not invalidating on deploy

Ensure your project generates `version.json` during the build phase. The cache uses `contentHash` from this file for version isolation.

### Disk cache growing too large

Old versions are automatically evicted (keeping 5 most recent). For manual cleanup:

```bash
rm -rf .putnami/cache/{project-name}
```

### Pattern deletion not working on disk cache

Due to key hashing, disk cache has limited pattern support. Patterns with wildcards clear the entire cache namespace. Use the layered cache (default) for better pattern support via the memory layer.
