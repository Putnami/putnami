import { afterAll, afterEach, beforeEach, describe, expect, it } from 'bun:test';
import { resetConfigLoader } from '@putnami/runtime';
import { application } from '../../src/application';
import { config } from '../../src/config';
import { http } from '../../src/http/http.plugin';
import { registerSessionStore, setActiveStoreService } from '../../src/session/session.store';
import { deleteSessionAll, setSession, useSession } from '../../src/session/session.utils';
import { AsyncMemorySessionStore } from './async-memory-store';

const ORIGINAL_CONFIG_DATA = process.env.CONFIG_DATA;
let store: AsyncMemorySessionStore;

registerSessionStore('async-memory', () => store);

function cookieOf(res: Response): string {
  return (res.headers.get('set-cookie') ?? '').split(';')[0];
}

async function withServer(run: (base: string) => Promise<void>): Promise<void> {
  const hp = http({ port: 0 });
  hp.route('GET', '/read', () => ({ value: useSession<string>('value') ?? null }));
  hp.route('GET', '/write', () => {
    setSession('value', 'kept');
    return { ok: true };
  });
  hp.route('GET', '/forget', () => {
    deleteSessionAll();
    return { ok: true };
  });
  hp.route('GET', '/fail', () => {
    setSession('value', 'written before the failure');
    throw new Error('handler failed');
  });
  const app = application().use(hp).use(config());
  await app.start();
  try {
    await run(`http://localhost:${hp.getServer()?.port}`);
  } finally {
    await app.stop();
  }
}

describe('an asynchronous session store behind the session API', () => {
  beforeEach(() => {
    store = new AsyncMemorySessionStore();
    setActiveStoreService(undefined);
    process.env.CONFIG_DATA = JSON.stringify({
      session: {
        store: 'async-memory',
        cookieName: 'async_sid',
        cookieSecret: 'a'.repeat(64),
        ttl: 300,
        secure: false,
      },
    });
    resetConfigLoader();
  });

  afterEach(() => {
    delete process.env.CONFIG_DATA;
    resetConfigLoader();
  });

  afterAll(() => {
    if (ORIGINAL_CONFIG_DATA === undefined) delete process.env.CONFIG_DATA;
    else process.env.CONFIG_DATA = ORIGINAL_CONFIG_DATA;
    resetConfigLoader();
  });

  it('keeps a value a handler wrote for the next request of the same browser', async () => {
    await withServer(async (base) => {
      const written = await fetch(`${base}/write`);
      const cookie = cookieOf(written);
      expect(cookie.startsWith('async_sid_id=')).toBe(true);
      expect(store.rows.size).toBe(1);

      const read = await fetch(`${base}/read`, { headers: { Cookie: cookie } });
      expect(await read.json()).toEqual({ value: 'kept' });
    });
  });

  it('reads the row once per request and writes nothing when the request changed nothing', async () => {
    await withServer(async (base) => {
      const cookie = cookieOf(await fetch(`${base}/write`));
      store.loads = 0;
      store.saves = 0;

      await fetch(`${base}/read`, { headers: { Cookie: cookie } });
      expect(store.loads).toBe(1);
      expect(store.saves).toBe(0);
    });
  });

  it('stores nothing for a visitor whose request only reads the session', async () => {
    await withServer(async (base) => {
      const read = await fetch(`${base}/read`);
      expect(await read.json()).toEqual({ value: null });
      expect(store.loads).toBe(0);
      expect(store.rows.size).toBe(0);
    });
  });

  it('deletes the row when a handler empties the session', async () => {
    await withServer(async (base) => {
      const cookie = cookieOf(await fetch(`${base}/write`));
      await fetch(`${base}/forget`, { headers: { Cookie: cookie } });
      expect(store.rows.size).toBe(0);
      expect(store.removes).toBe(1);

      const read = await fetch(`${base}/read`, { headers: { Cookie: cookie } });
      expect(await read.json()).toEqual({ value: null });
    });
  });

  it('still writes what a failed request changed before it failed', async () => {
    await withServer(async (base) => {
      const failed = await fetch(`${base}/fail`);
      expect(failed.status).toBe(500);
      expect([...store.rows.values()]).toEqual([{ value: 'written before the failure' }]);
    });
  });

  it('serves a request that started with an old copy the value a newer request wrote', async () => {
    await withServer(async (base) => {
      const cookie = cookieOf(await fetch(`${base}/write`));
      const [id] = [...store.rows.keys()];
      store.rows.set(id, { value: 'written by another request' });

      const read = await fetch(`${base}/read`, { headers: { Cookie: cookie } });
      expect(await read.json()).toEqual({ value: 'written by another request' });
    });
  });
});
