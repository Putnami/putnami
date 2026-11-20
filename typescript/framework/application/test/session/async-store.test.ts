import { afterAll, afterEach, beforeEach, describe, expect, it } from 'bun:test';
import { HttpException, resetConfigLoader, tryContext } from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import { application } from '../../src/application';
import { config } from '../../src/config';
import type { HttpMiddleware } from '../../src/http/http-middleware.type';
import { http } from '../../src/http/http.plugin';
import { registerSessionStore, setActiveStoreService } from '../../src/session/session.store';
import { deleteSessionAll, setSession, useSession } from '../../src/session/session.utils';
import { TelemetryCollector } from '../../src/telemetry/telemetry.collector';
import { TelemetryMiddleware } from '../../src/telemetry/telemetry.middleware';
import { clearCollector, setCollector } from '../../src/telemetry/telemetry.utils';
import { AsyncMemorySessionStore } from './async-memory-store';

const ORIGINAL_CONFIG_DATA = process.env.CONFIG_DATA;
let store: AsyncMemorySessionStore;

registerSessionStore('async-memory', () => store);

function cookieOf(res: Response): string {
  return (res.headers.get('set-cookie') ?? '').split(';')[0];
}

async function withServer(
  run: (base: string) => Promise<void>,
  collector?: TelemetryCollector,
  requestTimeoutMs?: number,
  middleware?: HttpMiddleware,
): Promise<void> {
  const hp = http({ port: 0, ...(requestTimeoutMs ? { requestTimeoutMs } : {}) });
  if (collector) hp.useOutermost(TelemetryMiddleware(collector));
  if (middleware) hp.use(middleware);
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

  specTest(
    'reads and writes the session under child spans of the request',
    {
      feature: 'typescript/service-telemetry',
      requirement: 'request-step-spans',
      check: 'session-load-and-commit-are-child-spans',
    },
    async () => {
      const collector = new TelemetryCollector();
      setCollector(collector);
      try {
        await withServer(async (base) => {
          const cookie = cookieOf(await fetch(`${base}/write`));
          collector.drainSpans();

          await fetch(`${base}/write`, { headers: { Cookie: cookie } });
          const spans = collector.drainSpans();
          const server = spans.find((span) => span.kind === 2);
          const load = spans.find((span) => span.name === 'session load');
          const commit = spans.find((span) => span.name === 'session commit');
          expect(server).toBeDefined();
          for (const step of [load, commit]) {
            expect(step?.kind).toBe(1);
            expect(step?.traceId).toBe(server?.traceId as string);
            expect(step?.parentSpanId).toBe(server?.spanId as string);
            expect(step?.statusCode).toBe(1);
          }
          // The request's span covers both steps.
          expect(BigInt(server?.startTimeUnixNano ?? 0)).toBeLessThanOrEqual(BigInt(load?.startTimeUnixNano ?? 0));
          expect(BigInt(server?.endTimeUnixNano ?? 0)).toBeGreaterThanOrEqual(BigInt(commit?.endTimeUnixNano ?? 0));
        }, collector);
      } finally {
        clearCollector(collector);
      }
    },
  );

  specTest(
    'ends a failed session step with error, and one its request stopped with canceled',
    {
      feature: 'typescript/service-telemetry',
      requirement: 'request-step-spans',
      check: 'a-failed-session-step-ends-error-or-canceled',
    },
    async () => {
      const collector = new TelemetryCollector();
      setCollector(collector);
      try {
        await withServer(
          async (base) => {
            const cookie = cookieOf(await fetch(`${base}/write`));
            collector.drainSpans();

            // A store of its own rejects with the request's stop reason at the timeout.
            store.loadGate = () =>
              new Promise((_, reject) => {
                const signal = tryContext<{ signal?: AbortSignal }>()?.signal;
                signal?.addEventListener('abort', () => reject(signal.reason));
              });
            await fetch(`${base}/read`, { headers: { Cookie: cookie } });
            await new Promise((resolve) => setTimeout(resolve, 50));
            const canceled = collector.drainSpans().find((span) => span.name === 'session load');
            expect(canceled?.statusCode).toBe(2);
            expect(canceled?.statusMessage).toBe('canceled');
            store.loadGate = undefined;

            // A store error while the request still runs is a failure, whatever its name.
            store.failNextLoad = Object.assign(new Error('Query aborted'), { name: 'QueryAbortError' });
            await fetch(`${base}/read`, { headers: { Cookie: cookie } });
            const loadFailed = collector.drainSpans().find((span) => span.name === 'session load');
            expect(loadFailed?.statusMessage).toBe('error');

            store.failNextSave = new Error('connection reset');
            await fetch(`${base}/fail`, { headers: { Cookie: cookie } });
            const failed = collector.drainSpans().find((span) => span.name === 'session commit');
            expect(failed?.statusCode).toBe(2);
            expect(failed?.statusMessage).toBe('error');
          },
          collector,
          100,
        );
      } finally {
        store.loadGate = undefined;
        clearCollector(collector);
      }
    },
  );

  specTest(
    'counts a request its timeout stopped in the session load as a 408',
    {
      feature: 'typescript/service-telemetry',
      requirement: 'request-step-spans',
      check: 'a-session-step-stopped-by-the-timeout-counts-as-408',
    },
    async () => {
      const collector = new TelemetryCollector();
      setCollector(collector);
      try {
        await withServer(
          async (base) => {
            const cookie = cookieOf(await fetch(`${base}/write`));
            collector.drainSpans();
            // The load waits on the database until the request's signal stops it.
            store.loadGate = () =>
              new Promise((_, reject) => {
                const signal = tryContext<{ signal?: AbortSignal }>()?.signal;
                signal?.addEventListener('abort', () =>
                  reject(Object.assign(new Error('Query aborted'), { name: 'QueryAbortError' })),
                );
              });
            const res = await fetch(`${base}/read`, { headers: { Cookie: cookie } });
            expect(res.status).toBe(408);
            await new Promise((resolve) => setTimeout(resolve, 50));
            const spans = collector.drainSpans();
            const server = spans.find((span) => span.kind === 2);
            expect(server?.attributes['http.response.status_code']).toBe(408);
            expect(server?.statusMessage).toBe('http.408');
            expect(spans.find((span) => span.name === 'session load')?.statusMessage).toBe('canceled');
          },
          collector,
          100,
        );
      } finally {
        store.loadGate = undefined;
        clearCollector(collector);
      }
    },
  );

  specTest(
    'counts a request that failed at its deadline, before the timeout fired, as a 408',
    {
      feature: 'typescript/service-telemetry',
      requirement: 'request-step-spans',
      check: 'a-step-stopped-at-the-deadline-counts-as-408',
    },
    async () => {
      let pastDeadline = false;
      const collector = new TelemetryCollector();
      setCollector(collector);
      try {
        await withServer(
          async (base) => {
            const cookie = cookieOf(await fetch(`${base}/write`));
            collector.drainSpans();
            // A call bound by the request deadline stops once it passed, while
            // the request's own timer has not fired yet.
            store.loadGate = async () => {
              const deadlineAt = tryContext<{ deadlineAt?: number }>()?.deadlineAt ?? 0;
              while (Date.now() < deadlineAt) {}
              throw Object.assign(new Error('deadline'), { name: 'ClientDeadlineError' });
            };
            const res = await fetch(`${base}/read`, { headers: { Cookie: cookie } });
            expect(res.status).toBe(408);
            await new Promise((resolve) => setTimeout(resolve, 50));
            const spans = collector.drainSpans();
            const server = spans.find((span) => span.kind === 2);
            expect(server?.attributes['http.response.status_code']).toBe(408);
            expect(spans.find((span) => span.name === 'session load')?.statusMessage).toBe('canceled');
            store.loadGate = undefined;

            // A middleware that fails once the deadline passed, before anything
            // ended the request, still counts as the 408 it answers.
            pastDeadline = true;
            const refused = await fetch(`${base}/read`);
            expect(refused.status).toBe(408);
            await new Promise((resolve) => setTimeout(resolve, 50));
            const refusedServer = collector.drainSpans().find((span) => span.kind === 2);
            expect(refusedServer?.attributes['http.response.status_code']).toBe(408);
            expect(refusedServer?.statusMessage).toBe('http.408');
          },
          collector,
          100,
          async (ctx, next) => {
            if (!pastDeadline) return next();
            while (Date.now() < (ctx.deadlineAt ?? 0)) {}
            throw new HttpException('refused', 401);
          },
        );
      } finally {
        store.loadGate = undefined;
        clearCollector(collector);
      }
    },
  );
});
