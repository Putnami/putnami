import type { CacheEntry, CacheStorage } from './cache-storage.interface';

/**
 * Deep-clone a cached value so callers cannot mutate the stored copy (and so
 * behavior matches the JSON-isolated disk layer). Falls back to the original
 * reference for values `structuredClone` cannot handle (e.g. functions).
 */
function isolate<T>(value: T): T {
  try {
    return structuredClone(value);
  } catch {
    return value;
  }
}

export class InMemoryCacheStorage implements CacheStorage {
  private storage = new Map<string, CacheEntry<unknown>>();
  private readonly maxSize: number;
  private readonly compiledPatterns = new Map<string, RegExp>();

  constructor(maxSize = 10_000) {
    this.maxSize = maxSize;
  }

  get<T>(key: string): Promise<CacheEntry<T> | undefined> {
    const entry = this.storage.get(key) as CacheEntry<T> | undefined;

    if (!entry) {
      return Promise.resolve(undefined);
    }

    // Check if entry has expired
    if (entry.expiresAt !== null && entry.expiresAt < Date.now()) {
      // Remove expired entry
      this.storage.delete(key);
      return Promise.resolve(undefined);
    }

    // Move to end for LRU ordering (Map preserves insertion order)
    this.storage.delete(key);
    this.storage.set(key, entry);

    // Return an isolated copy so a reader mutating the result cannot corrupt
    // the stored value (or any other reader's view of it).
    return Promise.resolve({ ...entry, value: isolate(entry.value) });
  }

  set<T>(key: string, value: T, ttl?: number): Promise<void> {
    // Delete first so re-insertion moves to end (LRU)
    this.storage.delete(key);

    // Evict oldest entry if at capacity
    if (this.storage.size >= this.maxSize) {
      const oldestKey = this.storage.keys().next().value;
      if (oldestKey !== undefined) {
        this.storage.delete(oldestKey);
      }
    }

    const expiresAt = ttl ? Date.now() + ttl : null;
    // Store an isolated copy so a producer mutating its reference after set()
    // cannot retroactively change the cached value.
    this.storage.set(key, { value: isolate(value), expiresAt });
    return Promise.resolve();
  }

  delete(key: string): Promise<void> {
    this.storage.delete(key);
    return Promise.resolve();
  }

  clear(): Promise<void> {
    this.storage.clear();
    return Promise.resolve();
  }

  /**
   * Get the number of cached entries (for testing/debugging)
   */
  size(): number {
    return this.storage.size;
  }

  /**
   * Delete all cache entries matching a glob pattern
   * Supports wildcards: * (any characters), ? (single character)
   * @returns Number of deleted entries
   */
  deletePattern(pattern: string): Promise<number> {
    const regex = this.globToRegex(pattern);
    let count = 0;

    for (const key of this.storage.keys()) {
      if (regex.test(key)) {
        this.storage.delete(key);
        count++;
      }
    }

    return Promise.resolve(count);
  }

  /**
   * Convert glob pattern to regular expression.
   * Compiled regexes are memoized to avoid recompilation for repeated
   * patterns. The memo is LRU-capped so dynamically built patterns
   * (e.g. derived from request data) cannot grow it without bound for
   * the lifetime of the process.
   */
  private globToRegex(pattern: string): RegExp {
    let re = this.compiledPatterns.get(pattern);
    if (re) {
      // Refresh recency: Map iteration order is insertion order.
      this.compiledPatterns.delete(pattern);
      this.compiledPatterns.set(pattern, re);
      return re;
    }
    const escaped = pattern
      .replace(/[.+^${}()|[\]\\]/g, '\\$&')
      .replace(/\*/g, '.*')
      .replace(/\?/g, '.');
    re = new RegExp(`^${escaped}$`);
    if (this.compiledPatterns.size >= InMemoryCacheStorage.MAX_COMPILED_PATTERNS) {
      const oldest = this.compiledPatterns.keys().next().value;
      if (oldest !== undefined) {
        this.compiledPatterns.delete(oldest);
      }
    }
    this.compiledPatterns.set(pattern, re);
    return re;
  }

  private static readonly MAX_COMPILED_PATTERNS = 256;
}
