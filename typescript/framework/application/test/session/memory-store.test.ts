import { afterEach, beforeEach, describe, expect, it } from 'bun:test';
import { resetConfigLoader } from '@putnami/runtime';
import { MemorySessionStore } from '../../src/session/memory.store';

const ORIGINAL_CONFIG_DATA = process.env.CONFIG_DATA;

beforeEach(() => {
  process.env.CONFIG_DATA = JSON.stringify({
    session: {
      cookieSecret: 'a'.repeat(64),
      store: 'memory',
      ttl: 2,
    },
  });
  resetConfigLoader();
});

afterEach(() => {
  if (ORIGINAL_CONFIG_DATA === undefined) {
    delete process.env.CONFIG_DATA;
  } else {
    process.env.CONFIG_DATA = ORIGINAL_CONFIG_DATA;
  }
  resetConfigLoader();
});

describe('MemorySessionStore', () => {
  it('set and get a value', () => {
    const store = new MemorySessionStore();
    store.set('s1', 'userId', 42);
    expect(store.get('s1', 'userId')).toBe(42);
    store.dispose();
  });

  it('returns undefined for missing key', () => {
    const store = new MemorySessionStore();
    expect(store.get('s1', 'missing')).toBeUndefined();
    store.dispose();
  });

  it('getAll returns all keys', () => {
    const store = new MemorySessionStore();
    store.set('s1', 'a', 1);
    store.set('s1', 'b', 2);
    expect(store.getAll('s1')).toEqual({ a: 1, b: 2 });
    store.dispose();
  });

  it('setAll replaces entire session', () => {
    const store = new MemorySessionStore();
    store.set('s1', 'keep', true);
    store.setAll('s1', { replaced: true });
    expect(store.getAll('s1')).toEqual({ replaced: true });
    store.dispose();
  });

  it('delete removes a single key', () => {
    const store = new MemorySessionStore();
    store.set('s1', 'a', 1);
    store.set('s1', 'b', 2);
    store.delete('s1', 'a');
    expect(store.get('s1', 'a')).toBeUndefined();
    expect(store.get('s1', 'b')).toBe(2);
    store.dispose();
  });

  it('deleteAll removes entire session', () => {
    const store = new MemorySessionStore();
    store.set('s1', 'a', 1);
    store.deleteAll('s1');
    expect(store.exists('s1')).toBe(false);
    expect(store.getAll('s1')).toEqual({});
    store.dispose();
  });

  it('exists returns true for active sessions', () => {
    const store = new MemorySessionStore();
    expect(store.exists('s1')).toBe(false);
    store.set('s1', 'key', 'val');
    expect(store.exists('s1')).toBe(true);
    store.dispose();
  });

  it('expires sessions after TTL', async () => {
    const store = new MemorySessionStore();
    store.set('s1', 'key', 'val');
    expect(store.exists('s1')).toBe(true);

    // TTL is 2 seconds
    await Bun.sleep(2100);
    expect(store.exists('s1')).toBe(false);
    expect(store.get('s1', 'key')).toBeUndefined();
    store.dispose();
  });

  it('isolates different session IDs', () => {
    const store = new MemorySessionStore();
    store.set('s1', 'key', 'one');
    store.set('s2', 'key', 'two');
    expect(store.get('s1', 'key')).toBe('one');
    expect(store.get('s2', 'key')).toBe('two');
    store.dispose();
  });

  it('dispose stops cleanup interval', () => {
    const store = new MemorySessionStore();
    store.dispose();
    // Calling dispose again should be safe
    store.dispose();
  });
});
