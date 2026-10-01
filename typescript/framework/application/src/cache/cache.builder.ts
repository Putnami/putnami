import type { CacheStorage } from './cache-storage.interface';
import { DiskCacheStorage } from './disk-cache.storage';
import { InMemoryCacheStorage } from './in-memory-cache.storage';
import { LayeredCacheStorage } from './layered-cache.storage';

// Global cache storage instance (singleton)
// In development mode, use memory-only cache (no disk persistence) so
// changes are reflected immediately on server restart.
// In production, use layered cache with memory (fast) and disk (persistent).
const isDevelopment = process.env.NODE_ENV !== 'production';
let globalCacheStorage: CacheStorage = isDevelopment
  ? new InMemoryCacheStorage()
  : new LayeredCacheStorage([new InMemoryCacheStorage(), new DiskCacheStorage('app')]);

/**
 * Single-flight de-duplication map. Concurrent `fetch()` callers for the same
 * cold key share one execution of the loader instead of all stampeding the
 * wrapped origin (DB/upstream). Keyed by the full cache key; the entry is
 * removed once the computation settles. Module-level so it is shared across the
 * per-call builder instances.
 */
const inFlight = new Map<string, Promise<unknown>>();

/**
 * Set the global cache storage implementation
 */
export function setCacheStorage(storage: CacheStorage): void {
  globalCacheStorage = storage;
}

/**
 * Get the global cache storage instance
 */
export function getCacheStorage(): CacheStorage {
  return globalCacheStorage;
}

/**
 * Evict cache entries matching a pattern
 * @param pattern - Glob pattern (e.g., 'user:*', 'Service:method:*')
 * @returns Number of evicted entries
 */
export async function evictCache(pattern: string): Promise<number> {
  return globalCacheStorage.deletePattern(pattern);
}

/**
 * Clear all cache entries
 */
export async function clearCache(): Promise<void> {
  return globalCacheStorage.clear();
}

/**
 * Fluent cache builder interface
 */
export interface CacheBuilder {
  /**
   * Set time-to-live for cached entries
   * @param ms - TTL in milliseconds
   */
  ttl(ms: number): CacheBuilder;

  /**
   * Set the cache key or key generator function
   * @param key - Static key string or function that generates key from arguments
   */
  for(key: string | ((...args: unknown[]) => string)): CacheBuilder;

  /**
   * Fetch data, using cache if available
   * @param fn - Function to execute if cache miss
   * @param args - Arguments to pass to the function (and key generator if dynamic)
   * @returns Cached or freshly computed result
   */
  fetch<R>(fn: (...args: unknown[]) => Promise<R> | R, ...args: unknown[]): Promise<R>;
}

class CacheBuilderImpl implements CacheBuilder {
  private _ttl?: number;
  private _key?: string | ((...args: unknown[]) => string);

  constructor(private readonly _name: string) {}

  ttl(ms: number): CacheBuilder {
    this._ttl = ms;
    return this;
  }

  for(key: string | ((...args: unknown[]) => string)): CacheBuilder {
    this._key = key;
    return this;
  }

  async fetch<R>(fn: (...args: unknown[]) => Promise<R> | R, ...args: unknown[]): Promise<R> {
    // Generate cache key
    const cacheKey = this.generateKey(args);

    // Try to get from cache
    const cachedEntry = await globalCacheStorage.get<R>(cacheKey);
    if (cachedEntry !== undefined) {
      return cachedEntry.value;
    }

    // Single-flight: if another caller is already computing this key, await
    // their in-flight result instead of running the loader again (prevents a
    // thundering herd against the origin on a cold key).
    const existing = inFlight.get(cacheKey) as Promise<R> | undefined;
    if (existing) {
      return existing;
    }

    const computation = (async () => {
      const result = await fn(...args);
      await globalCacheStorage.set(cacheKey, result, this._ttl);
      return result;
    })().finally(() => {
      inFlight.delete(cacheKey);
    });
    inFlight.set(cacheKey, computation);

    return computation;
  }

  private generateKey(args: unknown[]): string {
    const prefix = this._name;
    if (!this._key) {
      // Default key based on args
      return args.length > 0 ? `${prefix}:${JSON.stringify(args)}` : prefix;
    }

    if (typeof this._key === 'string') {
      return `${prefix}:${this._key}`;
    }

    // Dynamic key function
    return `${prefix}:${this._key(...args)}`;
  }
}

/**
 * Create a fluent cache builder
 *
 * @param name - Cache name identifier (e.g., 'loader', 'page', 'user')
 *
 * @example
 * // Basic usage with static key
 * const data = await cache('loader')
 *   .ttl(60000)
 *   .for('myKey')
 *   .fetch(() => fetchExpensiveData());
 *
 * @example
 * // Dynamic key from arguments
 * const user = await cache('user')
 *   .ttl(300000)
 *   .for((id: string) => `${id}`)
 *   .fetch((id) => fetchUser(id), userId);
 *
 * @example
 * // Without TTL (no expiration)
 * const config = await cache('config')
 *   .for('app')
 *   .fetch(() => loadConfig());
 */
export function cache(name: string): CacheBuilder {
  return new CacheBuilderImpl(name);
}
