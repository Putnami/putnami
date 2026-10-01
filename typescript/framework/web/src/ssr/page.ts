import {
  CacheMiddleware,
  type HttpCacheOptions,
  type HttpMiddleware,
  type HttpRequestContext,
} from '@putnami/application';
import type React from 'react';
import type { ClientSecurityRequirement } from '../shared/security.types';
import { withMiddleware } from './builder-mixin';
import { normalizeStatic, type StaticConfig, type StaticOptions } from './static';

// ---------------------------------------------------------------------------
// PageCacheOptions — extends HTTP cache with server-side SSR caching
// ---------------------------------------------------------------------------

/**
 * Cache options for pages.
 * Extends HTTP cache headers with server-side SSR caching support.
 */
export interface PageCacheOptions extends HttpCacheOptions {
  /**
   * Server-side cache TTL in milliseconds.
   * When set, SSR output is cached server-side.
   * The cache key includes the content hash, so cache is auto-invalidated on deploy.
   */
  ttl?: number;

  /**
   * Custom cache key generator.
   *
   * When not provided, the default key includes the route, params, query
   * parameters, and a fingerprint of the authenticated user (or `anon`). This
   * namespacing is required because cached SSR HTML embeds request-specific
   * data (security context roles/scopes and loader data); without it, one
   * request's rendered page could be served to another. Provide a custom key
   * only if you understand this hazard — e.g. for pages whose output never
   * varies by user or query string.
   *
   * @example
   * key: (ctx) => `page:${ctx.params.slug}`
   */
  key?: (ctx: HttpRequestContext) => string;
}

// ---------------------------------------------------------------------------
// PageDefinition — the object produced by page().render(Component)
// ---------------------------------------------------------------------------

const PAGE_MARKER = 'putnami:page' as const;

export interface PageDefinition {
  readonly __page: typeof PAGE_MARKER;
  readonly component: React.ComponentType;
  readonly middleware: readonly HttpMiddleware[];
  readonly statusCode?: number;
  readonly cache?: PageCacheOptions;
  /**
   * Static render configuration, populated when `.static()` was called.
   * When present the route is pre-rendered at build (SSG) and, when ISR
   * directives are supplied, revalidated on a TTL and/or cache-tag eviction.
   */
  readonly static?: StaticConfig;
  /**
   * Declarative security requirement, populated when `.secure()` was called.
   * Surfaced for the SSR generator so it can attach it to React Router's
   * `route.handle.security` (consumed for client-side route gating).
   */
  readonly security?: ClientSecurityRequirement;
}

export function isPageDefinition(value: unknown): value is PageDefinition {
  return typeof value === 'object' && value !== null && (value as PageDefinition).__page === PAGE_MARKER;
}

// ---------------------------------------------------------------------------
// PageBuilder — fluent API built by page()
// ---------------------------------------------------------------------------

class PageBuilderBase {
  _middleware: HttpMiddleware[] = [];
  _security?: ClientSecurityRequirement;
}

export class PageBuilder extends withMiddleware(PageBuilderBase) {
  private _statusCode?: number;
  private _cache?: PageCacheOptions;
  private _static?: StaticConfig;

  /** Set the HTTP status code for this page (e.g. 404 for not-found pages) */
  status(code: number): this {
    this._statusCode = code;
    return this;
  }

  /**
   * Set cache options for this page.
   *
   * - HTTP cache headers (Cache-Control, ETag) when `maxAge`, `etag`, etc. are set
   * - Server-side SSR caching when `ttl` is set
   *
   * @example
   * // Server-side cache only
   * page().cache({ ttl: 60000 }).render(MyPage)
   *
   * @example
   * // Combined with HTTP cache
   * page().cache({ ttl: 60000, maxAge: 30 }).render(MyPage)
   *
   * @example
   * // Custom cache key
   * page().cache({ ttl: 60000, key: (ctx) => `page:${ctx.params.slug}` }).render(MyPage)
   */
  cache(options: PageCacheOptions = {}): this {
    this._cache = options;
    // Add HTTP cache middleware for Cache-Control headers
    this._middleware.push(CacheMiddleware(options));
    return this;
  }

  /**
   * Render this page statically.
   *
   * The page is pre-rendered to HTML at build time (SSG) and served from the
   * static output with **zero client JavaScript** by default — only
   * `*.island.tsx` boundaries hydrate. The rendering mode is explicit and is
   * never inferred: a `.static()` route that touches request-scoped data is a
   * hard build error.
   *
   * @example
   * // Pure SSG — built once, served until the next build
   * page().static().render(MyPage)
   *
   * @example
   * // ISR — revalidate the static output every 60 seconds
   * page().static({ revalidate: 60 }).render(MyPage)
   *
   * @example
   * // ISR — revalidate when the `posts` cache tag is evicted
   * page().static({ revalidate: { tags: ['posts'] } }).render(MyPage)
   *
   * @example
   * // Dynamic route — enumerate concrete params at build (pure function)
   * page().static({ paths: async () => (await listPosts()).map((p) => ({ slug: p.slug })) }).render(PostPage)
   */
  static(options: StaticOptions = {}): this {
    this._static = normalizeStatic(options);
    return this;
  }

  /** Finalise the page definition with the page component */
  render(component: React.ComponentType): PageDefinition {
    return {
      __page: PAGE_MARKER,
      component,
      middleware: [...this._middleware],
      statusCode: this._statusCode,
      cache: this._cache,
      ...(this._static ? { static: this._static } : {}),
      ...(this._security ? { security: this._security } : {}),
    };
  }
}

// ---------------------------------------------------------------------------
// page() — entry point
// ---------------------------------------------------------------------------

/**
 * Declare a page with its configuration (security, CORS, rate limiting, cache)
 * and component in a single file.
 *
 * Export the result as the default export of your `page.tsx`.
 *
 * **Example — require authentication:**
 * ```ts
 * // src/app/dashboard/page.tsx
 * import { page } from '@putnami/web';
 *
 * export default page()
 *   .secure({ roles: ['user'] })
 *   .render(() => <DashboardPage />);
 * ```
 *
 * **Example — secure + rate limiting:**
 * ```ts
 * import { page } from '@putnami/web';
 *
 * export default page()
 *   .secure({ roles: ['user'] })
 *   .rateLimit({ max: 100 })
 *   .render(() => <MyPage />);
 * ```
 */
export function page(): PageBuilder {
  return new PageBuilder();
}
