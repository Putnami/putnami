import type { CacheEntry, CacheStorage } from './cache-storage.interface';

/**
 * Layered cache storage that combines multiple cache backends.
 * Uses write-through strategy: writes to all layers, reads from fastest available.
 *
 * Layer order matters: first layer is fastest (e.g., memory), last is slowest (e.g., disk).
 * On cache hit in a slower layer, faster layers are populated automatically.
 *
 * @example
 * const cache = new LayeredCacheStorage([
 *   new InMemoryCacheStorage(),  // L1: fast, volatile
 *   new DiskCacheStorage(),       // L2: slower, persistent
 * ]);
 */
export class LayeredCacheStorage implements CacheStorage {
  constructor(private readonly layers: CacheStorage[]) {
    if (layers.length === 0) {
      throw new Error('LayeredCacheStorage requires at least one layer');
    }
  }

  async get<T>(key: string): Promise<CacheEntry<T> | undefined> {
    // Try each layer in order (fastest first)
    for (let i = 0; i < this.layers.length; i++) {
      const entry = await this.layers[i].get<T>(key);

      if (entry !== undefined) {
        // Cache hit - populate faster layers that missed
        if (i > 0) {
          await this.populateFasterLayers(i, key, entry);
        }
        return entry;
      }
    }

    // Cache miss in all layers
    return undefined;
  }

  async set<T>(key: string, value: T, ttl?: number): Promise<void> {
    // Write-through: write to all layers in parallel
    await Promise.all(this.layers.map((layer) => layer.set(key, value, ttl)));
  }

  async delete(key: string): Promise<void> {
    // Delete from all layers in parallel
    await Promise.all(this.layers.map((layer) => layer.delete(key)));
  }

  async deletePattern(pattern: string): Promise<number> {
    // Delete from all layers, return max count
    const results = await Promise.all(this.layers.map((layer) => layer.deletePattern(pattern)));
    return Math.max(...results);
  }

  async clear(): Promise<void> {
    // Clear all layers in parallel
    await Promise.all(this.layers.map((layer) => layer.clear()));
  }

  /**
   * Populate faster cache layers when a hit is found in a slower layer.
   * This ensures subsequent reads are faster.
   */
  private async populateFasterLayers<T>(hitIndex: number, key: string, entry: CacheEntry<T>): Promise<void> {
    // Calculate remaining TTL if entry has expiration
    let ttl: number | undefined;
    if (entry.expiresAt !== null) {
      const remainingMs = entry.expiresAt - Date.now();
      if (remainingMs > 0) {
        ttl = remainingMs;
      } else {
        // Entry is expired, don't populate
        return;
      }
    }

    // Populate all faster layers (indices 0 to hitIndex-1)
    const fasterLayers = this.layers.slice(0, hitIndex);
    await Promise.all(fasterLayers.map((layer) => layer.set(key, entry.value, ttl)));
  }

  /**
   * Get the number of layers in this cache
   */
  layerCount(): number {
    return this.layers.length;
  }

  /**
   * Get a specific layer by index (for testing/debugging)
   */
  getLayer(index: number): CacheStorage | undefined {
    return this.layers[index];
  }
}
