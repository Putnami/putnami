import { describe, expect, it } from 'bun:test';
import { DEFAULT_MODULE_CACHE_SIZE, ReactModuleCache } from '../../src/ssr/react-module-cache';

describe('ReactModuleCache', () => {
  it('exposes the default cache size', () => {
    expect(DEFAULT_MODULE_CACHE_SIZE).toBe(500);
  });

  it('caches and returns the same promise for a key', async () => {
    const cache = new ReactModuleCache();
    let calls = 0;
    const load = () => {
      calls++;
      return Promise.resolve('value');
    };

    const a = cache.loadModule('k', load);
    const b = cache.loadModule('k', load);

    expect(a).toBe(b);
    expect(calls).toBe(1);
    expect(await a).toBe('value');
  });

  it('evicts the least-recently-used entry once the configured size is exceeded', async () => {
    const cache = new ReactModuleCache(2);
    const make = (v: string) => () => Promise.resolve(v);

    const first = cache.loadModule('a', make('a'));
    await first;
    cache.loadModule('b', make('b'));
    // Adding a third entry exceeds maxSize=2 and evicts the oldest ('a').
    cache.loadModule('c', make('c'));

    let reloaded = 0;
    const afterEviction = cache.loadModule('a', () => {
      reloaded++;
      return Promise.resolve('a2');
    });

    // 'a' was evicted, so the loader runs again and returns a fresh value.
    expect(reloaded).toBe(1);
    expect(afterEviction).not.toBe(first);
    expect(await afterEviction).toBe('a2');
  });

  it('clamps sizes below 1 so at least one entry is retained', async () => {
    const cache = new ReactModuleCache(0);
    const a = cache.loadModule('k', () => Promise.resolve('v'));
    expect(await a).toBe('v');
    // Same key still hits the cache (size clamped to 1, not 0).
    expect(cache.loadModule('k', () => Promise.resolve('other'))).toBe(a);
  });

  it('clears the cache entry when a module load fails so it can retry', async () => {
    const cache = new ReactModuleCache();
    await expect(cache.loadModule('boom', () => Promise.reject(new Error('fail')))).rejects.toThrow('fail');

    // Entry was cleared on failure: a retry runs the loader again and can succeed.
    const retry = cache.loadModule('boom', () => Promise.resolve('ok'));
    expect(await retry).toBe('ok');
  });

  it('clears page data when lazy page resolution fails so it can retry', async () => {
    const cache = new ReactModuleCache();
    let calls = 0;
    const Page = () => null;
    const lazyPage = async () => {
      calls++;
      if (calls === 1) throw new Error('page failed');
      return { default: Page };
    };

    await expect(cache.resolveLazyPageData('/boom', lazyPage, () => undefined)).rejects.toThrow('page failed');
    const retry = await cache.resolveLazyPageData('/boom', lazyPage, () => undefined);

    expect(calls).toBe(2);
    expect(retry.pageDef).toBeUndefined();
    expect(retry.middleware).toBeUndefined();
  });
});
