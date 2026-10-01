export type { CacheBuilder } from './cache.builder';
export { cache, clearCache, evictCache, getCacheStorage, setCacheStorage } from './cache.builder';
export type { CacheEntry, CacheStorage } from './cache-storage.interface';
export {
  DiskCacheStorage,
  evictOldVersionsInDir,
  getCacheBuildVersion,
  setCacheBuildVersion,
} from './disk-cache.storage';
export { InMemoryCacheStorage } from './in-memory-cache.storage';
export { LayeredCacheStorage } from './layered-cache.storage';
