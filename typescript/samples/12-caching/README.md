# Caching

Application-level and HTTP-level caching strategies, built on the framework cache.

## Features

- **Application-level cache**: the framework cache from `@putnami/application` (`getCacheStorage`, `evictCache`, `clearCache`) with TTL and pattern-based eviction
- **HTTP-level cache**: `Cache-Control` headers and `ETag` / `304 Not Modified` support
- Cache hit/miss statistics
- Admin endpoint for cache management — **demo-only and unauthenticated** so the sample runs standalone. The `GET`/`DELETE /admin` routes (especially the destructive cache eviction) must be gated with `.secure()` in a real application.

## Routes

### Application-Level Cache

Server-side cache backed by the framework cache — the application decides when to serve cached data.

| Method | Path | Description |
|--------|------|-------------|
| GET | `/products` | Cached product listing (30s TTL) |
| GET | `/products/[id]` | Cached single product (60s TTL) |
| GET | `/admin` | Cache statistics |
| DELETE | `/admin` | Evict cache entries (body: `{ "pattern": "products:*" }`) |

### HTTP-Level Cache

Browser/CDN cache — standard HTTP headers instruct clients when to reuse responses.

| Method | Path | Description |
|--------|------|-------------|
| GET | `/http/products` | Product listing with `Cache-Control: public, max-age=30` + ETag |
| GET | `/http/products/[id]` | Single product with `Cache-Control: private, max-age=60` + ETag |

### Other

| Method | Path | Description |
|--------|------|-------------|
| GET | `/healthz` | Aggregate health check |

## Run

```bash
putnami serve .
```

## Try It

Once the server is running, open a new terminal:

### Application-Level Cache

**1. Fetch a product (cache miss on first request):**

```bash
curl http://localhost:3912/products/prod-1
```

Expected response:

```json
{
  "product": { "id": "prod-1", "name": "...", "price": "..." },
  "source": "computed"
}
```

**2. Fetch the same product again (cache hit):**

```bash
curl http://localhost:3912/products/prod-1
```

Expected response — same product data, but `"source": "cache"`.

**3. List all products:**

```bash
curl http://localhost:3912/products
```

Expected response:

```json
{
  "products": [
    { "id": "...", "name": "...", "price": "..." },
    "..."
  ],
  "total": "...",
  "source": "computed"
}
```

Call again to see `"source": "cache"`.

**4. Check cache statistics:**

```bash
curl http://localhost:3912/admin
```

Expected response:

```json
{
  "cache": {
    "hits": 1,
    "misses": 2,
    "evictions": 0,
    "hitRate": 0.33
  }
}
```

**5. Evict product cache using a pattern:**

```bash
curl -X DELETE http://localhost:3912/admin \
  -H "Content-Type: application/json" \
  -d '{"pattern": "products:*"}'
```

Expected response:

```json
{
  "evicted": 2,
  "pattern": "products:*"
}
```

**6. Fetch the product again (cache miss after eviction):**

```bash
curl http://localhost:3912/products/prod-1
```

Expected response — `"source": "computed"` again (cache was evicted).

### HTTP-Level Cache

**7. Fetch products with HTTP cache headers:**

```bash
curl -v http://localhost:3912/http/products
```

Look for these response headers:

```
Cache-Control: public, max-age=30
ETag: "..."
```

The browser (or any HTTP client) will reuse this response for 30 seconds without contacting the server.

**8. Conditional request with ETag (304 Not Modified):**

```bash
# First request — get the ETag
ETAG=$(curl -s -D - http://localhost:3912/http/products/prod-1 | grep -i etag | tr -d '\r' | awk '{print $2}')

# Second request — send If-None-Match header
curl -v -H "If-None-Match: $ETAG" http://localhost:3912/http/products/prod-1
```

Expected: the server returns `304 Not Modified` with no body — the client's cached copy is still valid.

**9. Private cache with per-product ETag:**

```bash
curl -v http://localhost:3912/http/products/prod-2
```

Headers include `Cache-Control: private, max-age=60` — only the end-user's browser may cache this, not shared CDN proxies.

## Application vs HTTP Cache

| Aspect | Application Cache (`/products`) | HTTP Cache (`/http/products`) |
|---|---|---|
| **Where** | Server memory | Browser / CDN |
| **Control** | Application code (`getCacheStorage().get/set`) | HTTP headers (`Cache-Control`, `ETag`) |
| **Eviction** | Pattern-based (`evictCache('products:*')`) | TTL expiry or conditional revalidation |
| **304 support** | No | Yes (`If-None-Match` → `304 Not Modified`) |
| **Best for** | Expensive computations, DB queries | Static/semi-static API responses |

## Test

```bash
putnami test .
```

## What this sample proves

The two caches are different tools and the sample runs both side by side.
`src/api/products/` uses the framework cache — server memory, read and written
by application code, evicted by pattern — while `src/api/http/products/` uses
HTTP caching headers, where the browser or CDN holds the copy and `ETag`
revalidation turns a repeat request into a `304`.

The tests assert what each one actually guarantees: the framework cache returns
`undefined` on a miss and the stored entry on a hit, `evictCache('products:*')`
removes exactly the matching keys, and the metrics report hits, misses, evictions,
and a hit rate that starts at zero rather than at `NaN`.

Contract: the cache surface (`getCacheStorage`, `evictCache`, `clearCache`) is
owned by [`@putnami/application`](../../framework/application/README.md).
