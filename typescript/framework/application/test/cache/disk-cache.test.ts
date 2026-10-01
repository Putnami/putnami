import { mkdirSync, rmSync, writeFileSync } from 'node:fs';
import { afterEach, beforeEach, describe, expect, test } from 'bun:test';
import { joinPath } from '@putnami/utils';
import { DiskCacheStorage, evictOldVersionsInDir } from '../../src/cache/disk-cache.storage';

const TEST_CACHE_DIR = '/tmp/putnami-test-disk-cache';

describe('DiskCacheStorage', () => {
  let cache: DiskCacheStorage;

  beforeEach(() => {
    rmSync(TEST_CACHE_DIR, { recursive: true, force: true });
    cache = new DiskCacheStorage('test', undefined, TEST_CACHE_DIR);
  });

  afterEach(() => {
    rmSync(TEST_CACHE_DIR, { recursive: true, force: true });
  });

  test('set and get a value', async () => {
    await cache.set('key1', { name: 'test' });
    const entry = await cache.get<{ name: string }>('key1');
    expect(entry).toBeDefined();
    expect(entry?.value).toEqual({ name: 'test' });
    expect(entry?.expiresAt).toBeNull();
  });

  test('set with TTL and get before expiry', async () => {
    await cache.set('key2', 'value', 60_000);
    const entry = await cache.get<string>('key2');
    expect(entry).toBeDefined();
    expect(entry?.value).toBe('value');
    expect(entry?.expiresAt).toBeGreaterThan(Date.now());
  });

  test('returns undefined for expired entry', async () => {
    await cache.set('expired', 'value', 1); // 1ms TTL
    await Bun.sleep(10);
    const entry = await cache.get<string>('expired');
    expect(entry).toBeUndefined();
  });

  test('returns undefined for non-existent key', async () => {
    const entry = await cache.get<string>('nonexistent');
    expect(entry).toBeUndefined();
  });

  test('delete removes entry', async () => {
    await cache.set('to-delete', 'value');
    await cache.delete('to-delete');
    const entry = await cache.get<string>('to-delete');
    expect(entry).toBeUndefined();
  });

  test('delete non-existent key does not throw', async () => {
    await expect(cache.delete('nonexistent')).resolves.toBeUndefined();
  });

  test('clear removes all entries', async () => {
    await cache.set('a', 1);
    await cache.set('b', 2);
    await cache.clear();
    expect(await cache.get('a')).toBeUndefined();
    expect(await cache.get('b')).toBeUndefined();
  });

  test('size returns number of cached entries', async () => {
    expect(await cache.size()).toBe(0);
    await cache.set('a', 1);
    await cache.set('b', 2);
    expect(await cache.size()).toBe(2);
  });

  test('deletePattern with exact key', async () => {
    await cache.set('exact-key', 'value');
    const count = await cache.deletePattern('exact-key');
    expect(count).toBe(1);
    expect(await cache.get('exact-key')).toBeUndefined();
  });

  test('deletePattern with wildcard clears all', async () => {
    await cache.set('a', 1);
    await cache.set('b', 2);
    await cache.set('c', 3);
    const count = await cache.deletePattern('*');
    expect(count).toBe(3);
    expect(await cache.size()).toBe(0);
  });

  test('deletePattern with question mark clears all', async () => {
    await cache.set('x', 1);
    const count = await cache.deletePattern('?');
    expect(count).toBe(1);
  });

  test('deletePattern returns 0 for non-existent exact key', async () => {
    const count = await cache.deletePattern('nonexistent');
    expect(count).toBe(0);
  });

  test('handles corrupted cache file gracefully', async () => {
    // Write a corrupted file
    await cache.set('good', 'value');
    mkdirSync(TEST_CACHE_DIR, { recursive: true });

    // Create a corrupted file with a known hash
    const hasher = new Bun.CryptoHasher('sha256');
    hasher.update('corrupted-key');
    const hash = hasher.digest('hex').substring(0, 16);
    writeFileSync(joinPath(TEST_CACHE_DIR, `${hash}.json`), 'not-valid-json');

    const entry = await cache.get<string>('corrupted-key');
    expect(entry).toBeUndefined();
  });

  test('overwrite existing key', async () => {
    await cache.set('overwrite', 'first');
    await cache.set('overwrite', 'second');
    const entry = await cache.get<string>('overwrite');
    expect(entry?.value).toBe('second');
  });

  test('stores complex objects', async () => {
    const complex = {
      nested: { array: [1, 2, 3] },
      date: '2024-01-01',
      count: 42,
    };
    await cache.set('complex', complex);
    const entry = await cache.get<typeof complex>('complex');
    expect(entry?.value).toEqual(complex);
  });
});

describe('evictOldVersionsInDir', () => {
  const BASE_DIR = '/tmp/putnami-test-evict';

  beforeEach(() => {
    rmSync(BASE_DIR, { recursive: true, force: true });
  });

  afterEach(() => {
    rmSync(BASE_DIR, { recursive: true, force: true });
  });

  test('does nothing if directory does not exist', async () => {
    await expect(evictOldVersionsInDir('/tmp/nonexistent-evict-dir')).resolves.toBeUndefined();
  });

  test('does nothing when fewer than max versions', async () => {
    mkdirSync(joinPath(BASE_DIR, 'v1'), { recursive: true });
    mkdirSync(joinPath(BASE_DIR, 'v2'), { recursive: true });
    await evictOldVersionsInDir(BASE_DIR);
    // Both should still exist
    expect(Bun.file(joinPath(BASE_DIR, 'v1')).size).toBeDefined();
  });

  test('evicts oldest versions when exceeding max', async () => {
    // Create 7 version directories with staggered mtimes
    for (let i = 1; i <= 7; i++) {
      const dir = joinPath(BASE_DIR, `v${i}`);
      mkdirSync(dir, { recursive: true });
      // Write a file to ensure different mtimes
      writeFileSync(joinPath(dir, 'data.json'), `${i}`);
      // Small delay to ensure different mtimes
      if (i < 7) await Bun.sleep(10);
    }

    await evictOldVersionsInDir(BASE_DIR);

    // Should keep 5 most recent, remove 2 oldest
    const { readdirSync } = require('node:fs');
    const remaining = readdirSync(BASE_DIR) as string[];
    expect(remaining.length).toBe(5);
  });
});
