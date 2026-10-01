import { clearCache, evictCache, getCacheStorage } from '@putnami/application';
import { beforeEach, describe, expect, it } from 'bun:test';
import { metrics } from '../src/metrics';

beforeEach(async () => {
  await clearCache();
  metrics.reset();
});

describe('cache metrics', () => {
  it('reports a zero hit rate before any access', () => {
    expect(metrics.snapshot()).toEqual({ hits: 0, misses: 0, evictions: 0, hitRate: 0 });
  });

  it('tracks hits, misses, and hit rate', () => {
    metrics.recordHit();
    metrics.recordMiss();

    const stats = metrics.snapshot();
    expect(stats.hits).toBe(1);
    expect(stats.misses).toBe(1);
    expect(stats.hitRate).toBe(0.5);
  });

  it('counts evictions', () => {
    metrics.recordEvictions(2);
    metrics.recordEvictions(1);
    expect(metrics.snapshot().evictions).toBe(3);
  });
});

describe('framework cache (getCacheStorage)', () => {
  it('returns undefined on a miss and the stored entry on a hit', async () => {
    const storage = getCacheStorage();

    expect(await storage.get('products:all')).toBeUndefined();

    await storage.set('products:all', { total: 1 }, 10_000);
    const entry = await storage.get<{ total: number }>('products:all');
    expect(entry?.value.total).toBe(1);
  });

  it('evicts entries by pattern', async () => {
    const storage = getCacheStorage();
    await storage.set('products:1', 'a', 10_000);
    await storage.set('products:2', 'b', 10_000);
    await storage.set('users:1', 'c', 10_000);

    const evicted = await evictCache('products:*');
    expect(evicted).toBe(2);
    expect(await storage.get('products:1')).toBeUndefined();
    expect((await storage.get<string>('users:1'))?.value).toBe('c');
  });

  it('clears all entries', async () => {
    const storage = getCacheStorage();
    await storage.set('a', 1, 10_000);
    await storage.set('b', 2, 10_000);

    await clearCache();
    expect(await storage.get('a')).toBeUndefined();
    expect(await storage.get('b')).toBeUndefined();
  });
});
