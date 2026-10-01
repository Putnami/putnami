# Caching

Putnami provides a layered caching system for server-side data caching, plus HTTP cache header support for browser and CDN caching.

## Overview

The caching system supports:

- **Server-side caching** - In-memory and disk-based caching with TTL
- **HTTP cache headers** - `Cache-Control`, `ETag`, and `304 Not Modified` responses
- **Build version isolation** - Cache auto-invalidates on deployments
- **Cache eviction** - Pattern-based invalidation after mutations

## Server-side caching

### Loader cache

Add server-side caching to loaders using the `ttl` option:

```ts
// src/app/posts/loader.ts
import { loader } from '@putnami/web';

export default loader()
  .cache({ ttl: 60 * 60 * 1000 }) // Cache for 1 hour
  .handle(async (ctx) => {
    return { posts: await fetchPosts() };
  });
```

Cache keys are auto-generated from the route and parameters. For custom keys:

```ts
export default loader()
  .cache({
    ttl: 60000,
    key: (ctx) => `user:${ctx.params.id}`
  })
  .handle(async (ctx) => {
    return { user: await getUser(ctx.params.id) };
  });
```

### Page cache

Cache SSR output for pages:

```tsx
// src/app/posts/page.tsx
import { page } from '@putnami/web';

export default page()
  .cache({ ttl: 3600000 }) // Cache SSR for 1 hour
  .render(() => <PostsPage />);
```

### Direct cache API

For custom caching needs:

```ts
import { cache, evictCache, clearCache } from '@putnami/web';

// Cache expensive computation
const data = await cache('compute')
  .ttl(60000)
  .for('expensive-key')
  .fetch(() => computeExpensiveValue());

// With dynamic key from arguments
const user = await cache('users')
  .ttl(300000)
  .for((id: string) => id)
  .fetch(async (id) => fetchUser(id), userId);
```

## HTTP cache headers

Control browser and CDN caching with `Cache-Control` and `ETag`.

### Basic HTTP cache

```ts
import { loader } from '@putnami/web';

export default loader()
  .cache({ maxAge: 300, etag: true }) // 5 minutes browser cache
  .handle(async (ctx) => {
    return { posts: await fetchPosts() };
  });
```

When `etag: true` is set, the server computes an ETag and returns `304 Not Modified` for matching `If-None-Match` requests.

### Combined caching

For maximum performance, combine server-side and HTTP caching:

```ts
export default loader()
  .cache({
    ttl: 60 * 60 * 24 * 1000, // Server: 24 hours
    maxAge: 300,              // Browser: 5 minutes
    sMaxAge: 3600,            // CDN: 1 hour
    etag: true                // Enable conditional requests
  })
  .handle(async (ctx) => {
    return { data: await fetchData() };
  });
```

### Cache options

| Option                 | Description                                         |
| ---------------------- | --------------------------------------------------- |
| `ttl`                  | Server-side cache TTL in milliseconds               |
| `key`                  | Custom cache key generator `(ctx) => string`        |
| `maxAge`               | Sets `public, max-age=<value>` (seconds)            |
| `privateMaxAge`        | Sets `private, max-age=<value>` (seconds)           |
| `sMaxAge`              | Appends `s-maxage=<value>` for CDN (seconds)        |
| `staleWhileRevalidate` | Appends `stale-while-revalidate=<value>`            |
| `etag`                 | `true` for auto MD5 ETag, or `(body) => string`     |
| `cacheControl`         | Raw `Cache-Control` string (overrides shorthands)   |

## Cache eviction

### Eviction in actions

Actions can automatically evict cache entries after successful mutations:

```ts
// src/app/posts/action.ts
import { action } from '@putnami/web';

export default action()
  .evict('loader:/posts/*') // Evict all post loaders
  .handle(async (ctx) => {
    const formData = await ctx.req.formData();
    await createPost(formData);
    return { ok: true };
  });
```

### Multiple patterns

```ts
export default action()
  .evict(
    'loader:/posts/*',                 // Static pattern
    'page:/posts/*',                   // Page cache
    () => `user:${getCurrentUserId()}` // Dynamic pattern
  )
  .handle(async (ctx) => {
    // ... mutation logic
  });
```

### Manual eviction

```ts
import { evictCache, clearCache } from '@putnami/web';

// Evict by pattern
await evictCache('loader:/posts/*');
await evictCache('user:123');

// Clear all caches
await clearCache();
```

## Cache architecture

### Layered storage

The default configuration uses a two-layer architecture:

1. **Memory (L1)** - Fast, volatile in-memory storage
2. **Disk (L2)** - Slower but persistent file-based storage

```
Read: L1 → L2 → compute → write to both
Write: write to L1 and L2 simultaneously
```

On cache hit in L2, L1 is automatically populated for faster subsequent reads.

### Build version isolation

Cache directories include the build content hash:

```
.putnami/cache/{project}/{contentHash}/{cacheName}/
```

This ensures:

- Fresh cache after every deployment
- Safe rollbacks (each version has its own cache)
- No stale data from previous builds

Old cache versions are automatically evicted (keeping the last 5).

### Custom storage

Replace the default storage if needed:

```ts
import {
  setCacheStorage,
  InMemoryCacheStorage,
  DiskCacheStorage,
  LayeredCacheStorage
} from '@putnami/application';

// Memory only (no persistence)
setCacheStorage(new InMemoryCacheStorage());

// Disk only (persistent but slower)
setCacheStorage(new DiskCacheStorage('app'));

// Custom layered (default)
setCacheStorage(new LayeredCacheStorage([
  new InMemoryCacheStorage(),
  new DiskCacheStorage('app'),
]));
```

## Best practices

1. **Use server-side cache for expensive queries** - Database calls, API requests, computations
2. **Use HTTP cache for static content** - Pages that don't change often
3. **Combine both for maximum performance** - Server cache prevents recomputation, HTTP cache prevents requests
4. **Evict on mutations** - Use `.evict()` in actions to keep data fresh
5. **Use meaningful cache names** - Makes debugging and eviction easier
6. **Consider cache key design** - Include relevant parameters in custom keys

## Examples

### Blog with long cache

```ts
// loader.ts - Cache posts for 30 days server-side, 1 hour browser
export default loader()
  .cache({
    ttl: 60 * 60 * 24 * 30 * 1000,
    maxAge: 3600,
    etag: true
  })
  .handle(async (ctx) => {
    return { posts: await fetchPosts() };
  });

// action.ts - Evict cache when new post is created
export default action()
  .evict('loader:/blog/*', 'page:/blog/*')
  .handle(async (ctx) => {
    await createPost(ctx);
    return { ok: true };
  });
```

### User profile with short cache

```ts
export default loader()
  .cache({
    ttl: 60000, // 1 minute server cache
    maxAge: 0,  // No browser cache (user data)
    key: (ctx) => `user:${ctx.params.id}`
  })
  .handle(async (ctx) => {
    return { user: await getUser(ctx.params.id) };
  });
```

### API response caching

```ts
// In a loader or endpoint
import { cache } from '@putnami/web';

const apiData = await cache('external-api')
  .ttl(300000) // 5 minutes
  .for('weather-data')
  .fetch(async () => {
    const res = await fetch('https://api.weather.com/current');
    return res.json();
  });
```

## Related

- [Web](/docs/frameworks/typescript/web) - React SSR and routing
- [Forms and Actions](/docs/frameworks/typescript/forms-and-actions) - Handling mutations
- [HTTP and Middleware](/docs/frameworks/typescript/http-and-middleware) - Request handling
