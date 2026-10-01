import type { HttpMiddleware } from '@putnami/application';
import type { PageDefinition } from './page';
import type { LazyPageModule } from './module.types';
import { resolvePageOrDefinition } from './route-module.utils';
import { normalizeRoute } from './route-cache.utils';

/** Default maximum number of entries in module and page-data caches before LRU eviction. */
export const DEFAULT_MODULE_CACHE_SIZE = 500;

export type LazyPageData = {
  pageDef?: PageDefinition;
  middleware?: HttpMiddleware[];
  statusCode?: number;
};

export type MiddlewareResolver = (
  route: string,
  pageDef: PageDefinition | undefined,
  notFoundMiddleware: HttpMiddleware[] | undefined,
) => HttpMiddleware[] | undefined | Promise<HttpMiddleware[] | undefined>;

/**
 * LRU cache for lazy-loaded modules and resolved page data.
 * Prevents re-importing modules on each request and caches
 * the resolved page definitions with their middleware chains.
 */
export class ReactModuleCache {
  private readonly modules = new Map<string, Promise<unknown>>();
  private readonly pageData = new Map<string, Promise<LazyPageData>>();
  private readonly maxSize: number;

  /**
   * @param maxSize Maximum entries per cache before LRU eviction. Values below 1
   *   are clamped to 1 so the cache always retains the most recently used entry.
   */
  constructor(maxSize: number = DEFAULT_MODULE_CACHE_SIZE) {
    this.maxSize = Math.max(1, Math.floor(maxSize));
  }

  /**
   * Load a module lazily with LRU caching.
   * If the module fails to load, the cache entry is cleared so subsequent attempts can retry.
   */
  loadModule<T>(key: string, loader: () => Promise<T>): Promise<T> {
    const existing = this.modules.get(key);
    if (existing) {
      // LRU: move to end (most recently used)
      this.modules.delete(key);
      this.modules.set(key, existing);
      return existing as Promise<T>;
    }
    // Evict oldest entry if at capacity
    if (this.modules.size >= this.maxSize) {
      const oldest = this.modules.keys().next().value;
      if (oldest !== undefined) this.modules.delete(oldest);
    }
    const promise = loader().catch((error) => {
      // Clear cache on failure so subsequent requests can retry
      this.modules.delete(key);
      throw error;
    });
    this.modules.set(key, promise);
    return promise;
  }

  /**
   * Resolve lazy page data (definition + middleware) with LRU caching.
   */
  resolveLazyPageData(
    route: string,
    lazyPage: LazyPageModule,
    resolveMiddleware: MiddlewareResolver,
    notFoundMiddleware?: HttpMiddleware[],
  ): Promise<LazyPageData> {
    const cacheKey = `page-data:${normalizeRoute(route)}`;
    const existing = this.pageData.get(cacheKey);
    if (existing) {
      // LRU: move to end (most recently used)
      this.pageData.delete(cacheKey);
      this.pageData.set(cacheKey, existing);
      return existing;
    }
    // Evict oldest entry if at capacity
    if (this.pageData.size >= this.maxSize) {
      const oldest = this.pageData.keys().next().value;
      if (oldest !== undefined) this.pageData.delete(oldest);
    }

    const promise = (async () => {
      const pageModule = await this.loadModule(`page:${route}`, lazyPage);
      const { pageDef } = resolvePageOrDefinition(pageModule);
      const middleware = await resolveMiddleware(route, pageDef, notFoundMiddleware);
      return { pageDef, middleware, statusCode: pageDef?.statusCode };
    })().catch((error) => {
      this.pageData.delete(cacheKey);
      throw error;
    });

    this.pageData.set(cacheKey, promise);
    return promise;
  }
}
