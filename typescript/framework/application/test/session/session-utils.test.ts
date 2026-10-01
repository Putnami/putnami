import { afterEach, beforeEach, describe, expect, it } from 'bun:test';
import { resetConfigLoader, runInContext } from '@putnami/runtime';
import { MemorySessionStore } from '../../src/session/memory.store';
import {
  deleteSession,
  deleteSessionAll,
  setSession,
  setSessionAll,
  useSession,
  useSessionAll,
  useSessionId,
  useSessionStore,
} from '../../src/session/session.utils';
import { setActiveStoreService } from '../../src/session/session.store';
import type { HttpRequestContext } from '../../src/http';

// Ensure built-in stores are registered
import '../../src/session/cookie.store';
import '../../src/session/memory.store';

const ORIGINAL_CONFIG_DATA = process.env.CONFIG_DATA;
const SECRET = 'a'.repeat(64);

function setConfig(store: 'cookie' | 'memory' = 'cookie') {
  process.env.CONFIG_DATA = JSON.stringify({
    session: {
      cookieSecret: SECRET,
      store,
      ttl: 300,
    },
  });
  resetConfigLoader();
}

function createContext(cookie?: string): HttpRequestContext {
  const headers = new Headers(cookie ? { Cookie: cookie } : undefined);
  const req = new Request('https://app.test/path', { headers });
  return {
    body: async () => undefined,
    domain: () => 'app.test',
    headers: req.headers,
    host: () => 'app.test',
    method: 'GET',
    path: () => '/path',
    query: () => '',
    queryParams: () => ({}),
    req,
    secured: () => true,
    throw: (status: number) => {
      throw new Error(`throw ${status}`);
    },
    url: req.url,
  } as HttpRequestContext;
}

beforeEach(() => {
  setActiveStoreService(undefined);
  delete process.env.CONFIG_DATA;
  resetConfigLoader();
});

afterEach(() => {
  setActiveStoreService(undefined);
  if (ORIGINAL_CONFIG_DATA === undefined) {
    delete process.env.CONFIG_DATA;
  } else {
    process.env.CONFIG_DATA = ORIGINAL_CONFIG_DATA;
  }
  resetConfigLoader();
});

describe('useSessionStore', () => {
  it('returns cookie store when configured', () => {
    setConfig('cookie');
    const context = createContext();
    runInContext(context, () => {
      const store = useSessionStore();
      expect(store).toBeDefined();
    });
  });

  it('returns memory store when configured', () => {
    setConfig('memory');
    const context = createContext();
    runInContext(context, () => {
      const store = useSessionStore();
      expect(store).toBeInstanceOf(MemorySessionStore);
    });
  });
});

describe('useSessionId', () => {
  it('returns __cookie__ for cookie store', () => {
    setConfig('cookie');
    const context = createContext();
    const id = runInContext(context, () => useSessionId());
    expect(id).toBe('__cookie__');
  });

  it('generates UUID for memory store', () => {
    setConfig('memory');
    const context = createContext();
    const id = runInContext(context, () => useSessionId());
    expect(id).toMatch(/^[0-9a-f-]{36}$/);
  });

  it('reuses session ID from context on second call', () => {
    setConfig('memory');
    const context = createContext();
    const [id1, id2] = runInContext(context, () => {
      const a = useSessionId();
      const b = useSessionId();
      return [a, b];
    });
    expect(id1).toBe(id2);
  });
});

describe('session CRUD (cookie store)', () => {
  it('set and get a value', () => {
    setConfig('cookie');
    const context = createContext();
    runInContext(context, () => {
      setSession('userId', 42);
      expect(useSession<number>('userId')).toBe(42);
    });
  });

  it('deleteSession removes a key', () => {
    setConfig('cookie');
    const context = createContext();
    runInContext(context, () => {
      setSession('a', 1);
      setSession('b', 2);
      deleteSession('a');
      expect(useSession('a')).toBeUndefined();
      expect(useSession('b')).toBe(2);
    });
  });

  it('setSessionAll replaces all data', () => {
    setConfig('cookie');
    const context = createContext();
    runInContext(context, () => {
      setSession('old', true);
      setSessionAll({ replaced: true });
      expect(useSessionAll()).toEqual({ replaced: true });
    });
  });

  it('deleteSessionAll clears session', () => {
    setConfig('cookie');
    const context = createContext();
    runInContext(context, () => {
      setSession('key', 'val');
      deleteSessionAll();
      expect(useSessionAll()).toEqual({});
    });
  });
});
