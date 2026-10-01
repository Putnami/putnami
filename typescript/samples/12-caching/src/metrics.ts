// Application-level cache metrics.
//
// The cache itself is the framework cache from `@putnami/application`
// (`getCacheStorage`, `evictCache`, `clearCache`). The framework storage is
// intentionally backend-agnostic and does not track hit/miss counters, so this
// module records them at the application level — the kind of lightweight
// instrumentation the cache docs recommend ("Monitor cache hit rates").

let hits = 0;
let misses = 0;
let evictions = 0;

export const metrics = {
  recordHit(): void {
    hits++;
  },
  recordMiss(): void {
    misses++;
  },
  recordEvictions(count: number): void {
    evictions += count;
  },
  reset(): void {
    hits = 0;
    misses = 0;
    evictions = 0;
  },
  snapshot() {
    const total = hits + misses;
    return {
      hits,
      misses,
      evictions,
      hitRate: total > 0 ? hits / total : 0,
    };
  },
};
