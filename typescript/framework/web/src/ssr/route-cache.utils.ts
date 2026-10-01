import {
  buildCacheControl,
  cache,
  computeETag,
  evictCache,
  type HttpRequestContext,
  HttpResponse,
  type RouteHandler,
} from '@putnami/application';
import type { EvictPattern } from './action';
import type { LoaderCacheOptions } from './loader';
import type { PageCacheOptions } from './page';

export function normalizeRoute(route: string): string {
  if (!route.startsWith('/')) return `/${route}`;
  return route;
}

/**
 * Serialize route params into a stable cache key component.
 */
export function serializeParams(params: Record<string, string> | undefined): string {
  if (!params) return '';
  return Object.entries(params)
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([k, v]) => `${k}=${v}`)
    .join('&');
}

/**
 * Build a JSON response with HTTP cache headers (ETag, Cache-Control).
 * Returns early with 304 Not Modified if ETag matches.
 */
export function buildJsonResponseWithCache(
  data: unknown,
  ctx: HttpRequestContext,
  cacheOpts?: LoaderCacheOptions,
): HttpResponse {
  let response = HttpResponse.json(data, { status: 200 });

  // HTTP cache handling (ETag, Cache-Control)
  if (cacheOpts?.etag) {
    const body = JSON.stringify(data);
    const etag = typeof cacheOpts.etag === 'function' ? cacheOpts.etag(body) : computeETag(body);
    response = response.setHeader('ETag', etag);

    if (ctx.headers.get('If-None-Match') === etag) {
      return new HttpResponse(undefined, { status: 304, headers: { ETag: etag } });
    }
  }

  if (cacheOpts) {
    const cacheControl = buildCacheControl(cacheOpts);
    if (cacheControl) {
      response = response.setHeader('Cache-Control', cacheControl);
    }
  }

  return response;
}

/**
 * Execute a loader with optional server-side caching.
 */
export async function executeLoaderWithCache(
  loader: (ctx: HttpRequestContext) => unknown,
  ctx: HttpRequestContext,
  route: string,
  cacheOpts?: LoaderCacheOptions,
): Promise<unknown> {
  if (cacheOpts?.ttl) {
    const cacheKey = cacheOpts.key ? cacheOpts.key(ctx) : `${route}:${serializeParams(ctx.params)}`;
    return cache('loader')
      .ttl(cacheOpts.ttl)
      .for(cacheKey)
      .fetch(() => loader(ctx));
  }
  return loader(ctx);
}

/**
 * Evict cache patterns after action execution.
 */
export async function evictPatterns(patterns: readonly EvictPattern[] | undefined): Promise<void> {
  if (!patterns || patterns.length === 0) return;
  await Promise.all(
    patterns.map((pattern) => {
      const key = typeof pattern === 'function' ? pattern() : pattern;
      return evictCache(key);
    }),
  );
}

/**
 * Cached page data structure for SSR caching.
 */
interface CachedPageData {
  html: string;
  status: number;
}

/**
 * Per-identity component for the default page cache key.
 *
 * Cached SSR HTML embeds per-user data (`window.__securityContext` roles/scopes
 * and loader data), so a default key that ignored identity would serve one
 * user's rendered HTML to another (cross-user disclosure). Authenticated
 * requests are namespaced by a fingerprint of the resolved user; anonymous
 * requests share a single entry. Pages that need different sharing can supply
 * an explicit `key`.
 */
function pageIdentityKey(ctx: HttpRequestContext): string {
  const user = ctx.user;
  return user ? computeETag(stableSerialize(user)) : 'anon';
}

function pageQueryKey(ctx: HttpRequestContext): string {
  return serializeParams(ctx.queryParams());
}

function stableSerialize(value: unknown, seen = new WeakSet<object>()): string {
  if (value === null) return 'null';

  switch (typeof value) {
    case 'string':
    case 'number':
    case 'boolean':
      return JSON.stringify(value);
    case 'bigint':
      return JSON.stringify(value.toString());
    case 'undefined':
    case 'function':
    case 'symbol':
      return 'null';
  }

  if (value instanceof Date) {
    return JSON.stringify(value.toISOString());
  }

  if (seen.has(value)) {
    return JSON.stringify('[Circular]');
  }

  seen.add(value);
  if (Array.isArray(value)) {
    const serialized = `[${value.map((item) => stableSerialize(item, seen)).join(',')}]`;
    seen.delete(value);
    return serialized;
  }

  const record = value as Record<string, unknown>;
  const serialized = `{${Object.keys(record)
    .sort((a, b) => a.localeCompare(b))
    .map((key) => `${JSON.stringify(key)}:${stableSerialize(record[key], seen)}`)
    .join(',')}}`;
  seen.delete(value);
  return serialized;
}

/**
 * Wrap a page renderer with server-side caching when ttl is set.
 * SSR responses use ReadableStream which can only be consumed once,
 * so we need to read the HTML into a string for caching.
 */
export function wrapPageWithCache(
  renderer: RouteHandler,
  route: string,
  cacheOpts: PageCacheOptions | undefined,
): RouteHandler {
  const ttl = cacheOpts?.ttl;
  if (!ttl) {
    return renderer;
  }

  const keyFn = cacheOpts.key;
  return async (ctx) => {
    const cacheKey = keyFn
      ? keyFn(ctx)
      : `${route}:${serializeParams(ctx.params)}:${pageQueryKey(ctx)}:${pageIdentityKey(ctx)}`;

    try {
      const cachedData = await cache('page')
        .ttl(ttl)
        .for(cacheKey)
        .fetch<CachedPageData>(async () => {
          const responseHelper = (await renderer(ctx)) as HttpResponse;

          // Redirect responses must not be cached — they carry a Location header
          // that would be lost when we reconstruct the response from cached HTML.
          if (responseHelper.redirected) {
            throw responseHelper;
          }

          const status = responseHelper.status || 200;
          // Convert to Response and read the stream body into a string for caching
          const response = responseHelper.get();
          const html = await response.text();
          return { html, status };
        });

      // Return a new response with the cached HTML
      return new HttpResponse(cachedData.html, {
        status: cachedData.status,
        headers: { 'content-type': 'text/html;charset=utf-8' },
      });
    } catch (e) {
      // Redirect responses thrown from the cache factory — pass through uncached
      if (e instanceof HttpResponse) {
        return e;
      }
      throw e;
    }
  };
}
