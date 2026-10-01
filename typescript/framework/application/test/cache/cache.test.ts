import { mkdirSync, readdirSync, rmSync, utimesSync, writeFileSync } from 'node:fs';
import { joinPath } from '@putnami/utils';
import { afterEach, beforeEach, describe, expect, test } from 'bun:test';
import {
  cache,
  clearCache,
  DiskCacheStorage,
  evictCache,
  evictOldVersionsInDir,
  getCacheStorage,
  InMemoryCacheStorage,
  LayeredCacheStorage,
  setCacheStorage,
} from '../../src';

describe('Fluent Cache API', () => {
  beforeEach(() => {
    // Reset cache storage before each test
    setCacheStorage(new InMemoryCacheStorage());
  });

  test('should cache function results with static key', async () => {
    let callCount = 0;

    const getData = async () => {
      callCount++;
      return 'data';
    };

    // First call - should execute function
    const result1 = await cache('test').ttl(60_000).for('myKey').fetch(getData);
    expect(result1).toBe('data');
    expect(callCount).toBe(1);

    // Second call - should return cached result
    const result2 = await cache('test').ttl(60_000).for('myKey').fetch(getData);
    expect(result2).toBe('data');
    expect(callCount).toBe(1); // Still 1, not called again
  });

  test('should de-duplicate concurrent misses on the same cold key (single-flight)', async () => {
    let callCount = 0;
    const slowLoader = async () => {
      callCount++;
      await new Promise((resolve) => setTimeout(resolve, 30));
      return 'value';
    };

    // Fire N concurrent fetches for the same cold key before any set() lands.
    const results = await Promise.all(
      Array.from({ length: 8 }, () => cache('stampede').ttl(60_000).for('hot').fetch(slowLoader)),
    );

    expect(results).toEqual(Array(8).fill('value'));
    // The loader must run exactly once despite 8 concurrent callers.
    expect(callCount).toBe(1);
  });

  test('in-memory storage isolates cached values from caller mutation', async () => {
    const storage = new InMemoryCacheStorage();

    // Mutating after set() must not change the stored value.
    const original = { n: 1 };
    await storage.set('k1', original);
    original.n = 999;
    const afterSet = await storage.get<{ n: number }>('k1');
    expect(afterSet?.value.n).toBe(1);

    // Mutating a read result must not corrupt future readers.
    await storage.set('k2', { list: [1, 2] });
    const first = await storage.get<{ list: number[] }>('k2');
    first?.value.list.push(99);
    const second = await storage.get<{ list: number[] }>('k2');
    expect(second?.value.list).toEqual([1, 2]);
  });

  test('should cache function results with dynamic key', async () => {
    let callCount = 0;

    const fetchUser = async (id: string) => {
      callCount++;
      return { id, name: `User ${id}` };
    };

    // First call for user 1
    const user1 = await cache('user')
      .ttl(60_000)
      .for((id: string) => id)
      .fetch(fetchUser, '1');
    expect(user1).toEqual({ id: '1', name: 'User 1' });
    expect(callCount).toBe(1);

    // Second call for user 1 - cached
    const user1Again = await cache('user')
      .ttl(60_000)
      .for((id: string) => id)
      .fetch(fetchUser, '1');
    expect(user1Again).toEqual({ id: '1', name: 'User 1' });
    expect(callCount).toBe(1);

    // Call for user 2 - different key
    const user2 = await cache('user')
      .ttl(60_000)
      .for((id: string) => id)
      .fetch(fetchUser, '2');
    expect(user2).toEqual({ id: '2', name: 'User 2' });
    expect(callCount).toBe(2);
  });

  test('should expire cached entries after TTL', async () => {
    let callCount = 0;

    const getData = async () => {
      callCount++;
      return 'data';
    };

    // First call
    await cache('test').ttl(200).for('expiring').fetch(getData);
    expect(callCount).toBe(1);

    // Second call immediately - should be cached
    await cache('test').ttl(200).for('expiring').fetch(getData);
    expect(callCount).toBe(1);

    // Wait for TTL to expire
    await new Promise((resolve) => setTimeout(resolve, 400));

    // Third call after TTL - should execute again
    await cache('test').ttl(200).for('expiring').fetch(getData);
    expect(callCount).toBe(2);
  });

  test('should work without TTL (no expiration)', async () => {
    let callCount = 0;

    const getData = async () => {
      callCount++;
      return 'data';
    };

    // First call - no TTL specified
    await cache('test').for('permanent').fetch(getData);
    expect(callCount).toBe(1);

    // Should remain cached indefinitely
    await cache('test').for('permanent').fetch(getData);
    expect(callCount).toBe(1);
  });

  test('should use default key when .for() not called', async () => {
    let callCount = 0;

    const getData = async (value: string) => {
      callCount++;
      return value;
    };

    // First call with arg 'a'
    const result1 = await cache('test').ttl(60_000).fetch(getData, 'a');
    expect(result1).toBe('a');
    expect(callCount).toBe(1);

    // Same arg - should be cached
    const result2 = await cache('test').ttl(60_000).fetch(getData, 'a');
    expect(result2).toBe('a');
    expect(callCount).toBe(1);

    // Different arg - different default key
    const result3 = await cache('test').ttl(60_000).fetch(getData, 'b');
    expect(result3).toBe('b');
    expect(callCount).toBe(2);
  });

  test('should support synchronous functions', async () => {
    let callCount = 0;

    const computeSync = (a: number, b: number) => {
      callCount++;
      return a + b;
    };

    const result1 = await cache('compute')
      .ttl(60_000)
      .for((a: number, b: number) => `${a}:${b}`)
      .fetch(computeSync, 2, 3);
    expect(result1).toBe(5);
    expect(callCount).toBe(1);

    // Cached
    const result2 = await cache('compute')
      .ttl(60_000)
      .for((a: number, b: number) => `${a}:${b}`)
      .fetch(computeSync, 2, 3);
    expect(result2).toBe(5);
    expect(callCount).toBe(1);
  });
});

describe('Cache Eviction', () => {
  beforeEach(() => {
    setCacheStorage(new InMemoryCacheStorage());
  });

  test('evictCache should remove entries matching pattern', async () => {
    const storage = getCacheStorage();

    await storage.set('user:1', 'value1');
    await storage.set('user:2', 'value2');
    await storage.set('product:1', 'value3');

    // Evict all user caches
    const count = await evictCache('user:*');
    expect(count).toBe(2);

    // Verify only user caches were deleted
    expect(await storage.get('user:1')).toBeUndefined();
    expect(await storage.get('user:2')).toBeUndefined();
    expect(await storage.get('product:1')).toBeDefined();
  });

  test('clearCache should remove all entries', async () => {
    const storage = getCacheStorage();

    await storage.set('key1', 'value1');
    await storage.set('key2', 'value2');
    await storage.set('key3', 'value3');

    await clearCache();

    expect(await storage.get('key1')).toBeUndefined();
    expect(await storage.get('key2')).toBeUndefined();
    expect(await storage.get('key3')).toBeUndefined();
  });

  test('should evict and refetch', async () => {
    let callCount = 0;
    let currentValue = 'initial';

    const getValue = async () => {
      callCount++;
      return currentValue;
    };

    // First fetch
    const result1 = await cache('test').ttl(60_000).for('mutable').fetch(getValue);
    expect(result1).toBe('initial');
    expect(callCount).toBe(1);

    // Change underlying value
    currentValue = 'updated';

    // Still cached
    const result2 = await cache('test').ttl(60_000).for('mutable').fetch(getValue);
    expect(result2).toBe('initial');
    expect(callCount).toBe(1);

    // Evict (cache key is now prefixed with cache name)
    await evictCache('test:mutable');

    // Now gets new value
    const result3 = await cache('test').ttl(60_000).for('mutable').fetch(getValue);
    expect(result3).toBe('updated');
    expect(callCount).toBe(2);
  });
});

describe('InMemoryCacheStorage', () => {
  test('should get and set values', async () => {
    const storage = new InMemoryCacheStorage();

    await storage.set('key1', 'value1');
    const entry = await storage.get('key1');

    expect(entry).toBeDefined();
    expect(entry?.value).toBe('value1');
    expect(entry?.expiresAt).toBeNull();
  });

  test('should handle TTL', async () => {
    const storage = new InMemoryCacheStorage();

    await storage.set('key1', 'value1', 200); // 200ms TTL

    const entry1 = await storage.get('key1');
    expect(entry1).toBeDefined();

    await new Promise((resolve) => setTimeout(resolve, 400));

    const entry2 = await storage.get('key1');
    expect(entry2).toBeUndefined(); // Expired
  });

  test('should delete values', async () => {
    const storage = new InMemoryCacheStorage();

    await storage.set('key1', 'value1');
    await storage.delete('key1');

    const entry = await storage.get('key1');
    expect(entry).toBeUndefined();
  });

  test('should clear all values', async () => {
    const storage = new InMemoryCacheStorage();

    await storage.set('key1', 'value1');
    await storage.set('key2', 'value2');
    await storage.clear();

    expect(await storage.get('key1')).toBeUndefined();
    expect(await storage.get('key2')).toBeUndefined();
    expect(storage.size()).toBe(0);
  });

  test('should delete pattern', async () => {
    const storage = new InMemoryCacheStorage();

    await storage.set('Service:method1:[]', 'value1');
    await storage.set('Service:method2:[]', 'value2');
    await storage.set('Other:method:[]', 'value3');

    const count = await storage.deletePattern('Service:*');
    expect(count).toBe(2);

    expect(await storage.get('Service:method1:[]')).toBeUndefined();
    expect(await storage.get('Other:method:[]')).toBeDefined();
  });
});

describe('DiskCacheStorage', () => {
  const testCacheDir = '.putnami/test-cache';

  afterEach(() => {
    try {
      rmSync(testCacheDir, { recursive: true, force: true });
    } catch {
      // Ignore cleanup errors
    }
  });

  test('should get and set values', async () => {
    const storage = new DiskCacheStorage('test', undefined, testCacheDir);

    await storage.set('key1', 'value1');
    const entry = await storage.get('key1');

    expect(entry).toBeDefined();
    expect(entry?.value).toBe('value1');
    expect(entry?.expiresAt).toBeNull();
  });

  test('should handle TTL', async () => {
    const storage = new DiskCacheStorage('test', undefined, testCacheDir);

    await storage.set('key1', 'value1', 200);

    const entry1 = await storage.get('key1');
    expect(entry1).toBeDefined();

    await new Promise((resolve) => setTimeout(resolve, 400));

    const entry2 = await storage.get('key1');
    expect(entry2).toBeUndefined();
  });

  test('should persist values across instances', async () => {
    const storage1 = new DiskCacheStorage('test', undefined, testCacheDir);
    await storage1.set('key1', { data: 'complex value' });

    const storage2 = new DiskCacheStorage('test', undefined, testCacheDir);
    const entry = await storage2.get<{ data: string }>('key1');

    expect(entry).toBeDefined();
    expect(entry?.value.data).toBe('complex value');
  });
});

describe('Cache Version Eviction', () => {
  const testBaseDir = '.putnami/test-eviction-cache';

  afterEach(() => {
    try {
      rmSync(testBaseDir, { recursive: true, force: true });
    } catch {
      // Ignore cleanup errors
    }
  });

  test('should evict old versions keeping only last 5', async () => {
    // Simulate 7 version directories with different timestamps
    const versionDirs = ['v1', 'v2', 'v3', 'v4', 'v5', 'v6', 'v7'];
    const baseTime = Date.now() - 10_000;

    for (let i = 0; i < versionDirs.length; i++) {
      const versionDir = joinPath(testBaseDir, versionDirs[i], 'test-cache');
      mkdirSync(versionDir, { recursive: true });
      writeFileSync(joinPath(versionDir, 'test.json'), '{}');

      // Set mtime to simulate age (v1 oldest, v7 newest)
      const mtime = new Date(baseTime + i * 1000);
      utimesSync(joinPath(testBaseDir, versionDirs[i]), mtime, mtime);
    }

    // Verify all 7 versions exist
    expect(readdirSync(testBaseDir).length).toBe(7);

    // Create a new version (v8) which should trigger eviction
    const newVersionDir = joinPath(testBaseDir, 'v8', 'test-cache');
    mkdirSync(newVersionDir, { recursive: true });
    writeFileSync(joinPath(newVersionDir, 'test.json'), '{}');

    // Call eviction (simulating what happens during cache init)
    await evictOldVersionsInDir(testBaseDir);

    // Verify only 5 versions remain
    const remainingVersions = readdirSync(testBaseDir);
    expect(remainingVersions.length).toBe(5);

    // Verify the oldest versions (v1, v2, v3) were removed and newest (v4-v8) kept
    expect(remainingVersions).not.toContain('v1');
    expect(remainingVersions).not.toContain('v2');
    expect(remainingVersions).not.toContain('v3');
    expect(remainingVersions).toContain('v8');
  });

  test('should not evict when fewer than 5 versions', async () => {
    // Create only 3 versions
    for (const version of ['v1', 'v2', 'v3']) {
      const versionDir = joinPath(testBaseDir, version, 'test-cache');
      mkdirSync(versionDir, { recursive: true });
    }

    await evictOldVersionsInDir(testBaseDir);

    // All 3 should remain
    expect(readdirSync(testBaseDir).length).toBe(3);
  });

  test('should handle non-existent directory gracefully', async () => {
    // Should not throw
    await expect(evictOldVersionsInDir('/non/existent/path')).resolves.toBeUndefined();
  });
});

describe('LayeredCacheStorage', () => {
  test('should require at least one layer', () => {
    expect(() => new LayeredCacheStorage([])).toThrow('LayeredCacheStorage requires at least one layer');
  });

  test('should read from fastest layer first', async () => {
    const memory = new InMemoryCacheStorage();
    const disk = new InMemoryCacheStorage();

    const layered = new LayeredCacheStorage([memory, disk]);

    await memory.set('key1', 'from-memory');

    const entry = await layered.get('key1');
    expect(entry?.value).toBe('from-memory');
  });

  test('should write to all layers (write-through)', async () => {
    const memory = new InMemoryCacheStorage();
    const disk = new InMemoryCacheStorage();

    const layered = new LayeredCacheStorage([memory, disk]);

    await layered.set('key1', 'value1');

    expect((await memory.get('key1'))?.value).toBe('value1');
    expect((await disk.get('key1'))?.value).toBe('value1');
  });

  test('should populate faster layers on cache hit in slower layer', async () => {
    const memory = new InMemoryCacheStorage();
    const disk = new InMemoryCacheStorage();

    const layered = new LayeredCacheStorage([memory, disk]);

    // Set only in disk (slower layer)
    await disk.set('key1', 'from-disk');

    expect(await memory.get('key1')).toBeUndefined();

    // Get through layered cache
    const entry = await layered.get('key1');
    expect(entry?.value).toBe('from-disk');

    // Now memory should be populated
    expect((await memory.get('key1'))?.value).toBe('from-disk');
  });

  test('should preserve TTL when populating faster layers', async () => {
    const memory = new InMemoryCacheStorage();
    const disk = new InMemoryCacheStorage();

    const layered = new LayeredCacheStorage([memory, disk]);

    await disk.set('key1', 'value1', 200);

    await new Promise((resolve) => setTimeout(resolve, 50));

    await layered.get('key1');

    const memEntry = await memory.get('key1');
    expect(memEntry?.value).toBe('value1');
    expect(memEntry?.expiresAt).toBeDefined();
    if (memEntry?.expiresAt) {
      expect(memEntry.expiresAt).toBeLessThan(Date.now() + 200);
    }
  });

  test('should work with fluent cache API', async () => {
    const memory = new InMemoryCacheStorage();
    const disk = new InMemoryCacheStorage();

    setCacheStorage(new LayeredCacheStorage([memory, disk]));

    let callCount = 0;

    const fetchData = async (id: string) => {
      callCount++;
      return `data-${id}`;
    };

    // First call - executes function, caches in both layers
    const result = await cache('data')
      .ttl(60_000)
      .for((id: string) => id)
      .fetch(fetchData, '1');
    expect(result).toBe('data-1');
    expect(callCount).toBe(1);

    // Cache key is now 'data:1' (cache name + key)
    expect((await memory.get('data:1'))?.value).toBe('data-1');
    expect((await disk.get('data:1'))?.value).toBe('data-1');

    // Second call - returns from cache
    await cache('data')
      .ttl(60_000)
      .for((id: string) => id)
      .fetch(fetchData, '1');
    expect(callCount).toBe(1);
  });
});
