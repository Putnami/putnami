/** A cached value along with its expiration timestamp. */
export interface CacheEntry<T> {
  value: T;
  expiresAt: number | null; // null means no expiration
}

/** Backend-agnostic interface for key-value cache operations with TTL and pattern-based deletion. */
export interface CacheStorage {
  get<T>(key: string): Promise<CacheEntry<T> | undefined>;
  set<T>(key: string, value: T, ttl?: number): Promise<void>;
  delete(key: string): Promise<void>;
  deletePattern(pattern: string): Promise<number>; // Returns count of deleted entries
  clear(): Promise<void>;
}
