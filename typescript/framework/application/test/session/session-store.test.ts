import { afterEach, beforeEach, describe, expect, it } from 'bun:test';
import { resetConfigLoader } from '@putnami/runtime';
import {
  SessionStoreService,
  disposeRegisteredStores,
  getRegisteredStore,
  registerSessionStore,
  setActiveStoreService,
  type SessionStore,
} from '../../src/session/session.store';

// Import built-in stores to ensure they're registered
import '../../src/session/cookie.store';
import '../../src/session/memory.store';

const ORIGINAL_CONFIG_DATA = process.env.CONFIG_DATA;

beforeEach(() => {
  process.env.CONFIG_DATA = JSON.stringify({
    session: {
      cookieSecret: 'a'.repeat(64),
      store: 'memory',
      ttl: 300,
    },
  });
  resetConfigLoader();
  setActiveStoreService(undefined);
});

afterEach(() => {
  if (ORIGINAL_CONFIG_DATA === undefined) {
    delete process.env.CONFIG_DATA;
  } else {
    process.env.CONFIG_DATA = ORIGINAL_CONFIG_DATA;
  }
  resetConfigLoader();
  setActiveStoreService(undefined);
});

function createMockStore(): SessionStore {
  const data = new Map<string, Record<string, unknown>>();
  return {
    get: <T>(sid: string, key: string) => data.get(sid)?.[key] as T | undefined,
    set: <T>(sid: string, key: string, value: T) => {
      if (!data.has(sid)) data.set(sid, {});
      data.get(sid)![key] = value;
    },
    delete: (sid: string, key: string) => {
      const s = data.get(sid);
      if (s) delete s[key];
    },
    getAll: <S>(sid: string) => (data.get(sid) ?? {}) as S,
    setAll: <S>(sid: string, session: S) => {
      data.set(sid, session as Record<string, unknown>);
    },
    deleteAll: (sid: string) => {
      data.delete(sid);
    },
    exists: (sid: string) => data.has(sid),
  };
}

describe('registerSessionStore + getRegisteredStore', () => {
  it('registers and retrieves a custom store', () => {
    const mockStore = createMockStore();
    registerSessionStore('test-custom', () => mockStore);
    const store = getRegisteredStore('test-custom');
    expect(store).toBe(mockStore);
  });

  it('caches instances on second call', () => {
    let callCount = 0;
    registerSessionStore('test-counted', () => {
      callCount++;
      return createMockStore();
    });
    const first = getRegisteredStore('test-counted');
    const second = getRegisteredStore('test-counted');
    expect(first).toBe(second);
    expect(callCount).toBe(1);
  });

  it('throws for unknown store name', () => {
    expect(() => getRegisteredStore('nonexistent-store')).toThrow(/Unknown session store.*nonexistent-store/);
  });

  it('retrieves built-in cookie store', () => {
    const store = getRegisteredStore('cookie');
    expect(store).toBeDefined();
  });

  it('retrieves built-in memory store', () => {
    const store = getRegisteredStore('memory');
    expect(store).toBeDefined();
  });
});

describe('SessionStoreService', () => {
  it('creates and caches store instances', () => {
    const service = new SessionStoreService();
    const mockStore = createMockStore();
    registerSessionStore('test-svc', () => mockStore);

    setActiveStoreService(service);
    const store = getRegisteredStore('test-svc');
    expect(store).toBe(mockStore);

    // Second call returns cached
    const again = getRegisteredStore('test-svc');
    expect(again).toBe(store);
    service.close();
  });

  it('throws for unknown store', () => {
    const service = new SessionStoreService();
    setActiveStoreService(service);
    expect(() => getRegisteredStore('does-not-exist')).toThrow(/Unknown session store/);
    service.close();
  });

  it('close clears cache and deactivates', () => {
    const service = new SessionStoreService();
    setActiveStoreService(service);

    let callCount = 0;
    registerSessionStore('test-close', () => {
      callCount++;
      return createMockStore();
    });

    getRegisteredStore('test-close');
    expect(callCount).toBe(1);

    service.close();

    // After close, getRegisteredStore falls back to module cache
    getRegisteredStore('test-close');
    // Factory called again because service cache was cleared and module-level takes over
    expect(callCount).toBe(2);
  });

  it('disposes store instances on close', () => {
    const service = new SessionStoreService();
    setActiveStoreService(service);

    let disposed = 0;
    const store = { ...createMockStore(), dispose: () => disposed++ };
    registerSessionStore('test-dispose', () => store);

    getRegisteredStore('test-dispose');
    service.close();

    expect(disposed).toBe(1);
  });
});

describe('disposeRegisteredStores', () => {
  it('disposes and clears the module-level fallback cache', () => {
    setActiveStoreService(undefined);

    let disposed = 0;
    const store = { ...createMockStore(), dispose: () => disposed++ };
    registerSessionStore('test-module-dispose', () => store);

    // Populate the module-level cache (no active service).
    getRegisteredStore('test-module-dispose');

    disposeRegisteredStores();
    expect(disposed).toBe(1);
  });
});
