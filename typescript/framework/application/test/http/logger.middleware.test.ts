import { afterEach, describe, expect, it } from 'bun:test';
import { Logger, observeLogRecords, resetLoggerConfig, runInContext, setLogLevel, useLogger } from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import { MemoryLogger, MemorySink } from '@putnami/runtime/testing';
import { application } from '../../src/application';
import { buildHttpContext } from '../../src/http/http-context.builder';
import type { HttpRequestContextInternal } from '../../src/http/http-context.type';
import { HttpResponse } from '../../src/http/http-response';
import { http } from '../../src/http/http.plugin';
import { LoggerMiddleware } from '../../src/http/logger.middleware';

async function runWithLogger(
  context: Record<string, unknown>,
  next: () => Promise<HttpResponse | undefined>,
): Promise<MemoryLogger> {
  const logger = new MemoryLogger();
  const runtimeContext = { ...context, logger };
  const middleware = LoggerMiddleware();
  await runInContext(runtimeContext, () => middleware(runtimeContext as never, next));
  return logger;
}

describe('LoggerMiddleware', () => {
  it('handles a real request context without crashing (regression: destructured path() lost this)', async () => {
    const plugin = http({ port: 0 });
    plugin.use(LoggerMiddleware());
    plugin.get('/items', () => HttpResponse.json({ ok: true }));
    const app = application().use(plugin);
    await app.start();
    const res = await fetch(`http://localhost:${plugin.getServer()?.port}/items`);
    expect(res.status).toBe(200);
    expect(await res.json()).toEqual({ ok: true });
    await app.stop();
  });

  it('logs and rejects when the next middleware throws before returning a promise', async () => {
    const logger = new MemoryLogger();
    const context = { method: 'GET', route: '/boom', path: () => '/boom', logger };
    const middleware = LoggerMiddleware();
    const failure = new Error('thrown synchronously');
    const outcome = await runInContext(context, () =>
      Promise.resolve(
        middleware(context as never, () => {
          throw failure;
        }),
      ).then(
        () => 'resolved',
        (error: unknown) => error,
      ),
    );
    expect(outcome).toBe(failure);
    const errors = logger.entries.filter((entry) => entry.level === 'error');
    expect(errors).toHaveLength(1);
    expect(errors[0]?.message).toBe('[GET] /boom');
    const payload = errors[0]?.context?.['http'] as Record<string, unknown> | undefined;
    expect(payload?.['status']).toBe(500);
  });

  it('rejects, and resolves a plain value, on an excluded route as on any other', async () => {
    const middleware = LoggerMiddleware({ exclude: ['/health'] });
    const context = { method: 'GET', route: '/health', path: () => '/health', logger: new MemoryLogger() };
    const failure = new Error('thrown synchronously');
    const thrown = await runInContext(context, () =>
      middleware(context as never, () => {
        throw failure;
      }).then(
        () => 'resolved',
        (error: unknown) => error,
      ),
    );
    expect(thrown).toBe(failure);
    const plain = new HttpResponse('ok');
    const resolved = await runInContext(context, () =>
      middleware(context as never, (() => plain) as unknown as () => Promise<HttpResponse>),
    );
    expect(resolved).toBe(plain);
  });

  it('should log implicit HttpResponse status as 200', async () => {
    const logger = await runWithLogger(
      {
        method: 'GET',
        route: '/react/chunk.panpew1h.js',
        path: () => '/react/chunk.panpew1h.js',
      },
      async () => new HttpResponse('ok'),
    );

    expect(logger.entries).toHaveLength(1);
    const entry = logger.entries[0];
    const payload = entry?.context?.['http'] as Record<string, unknown>;
    expect(payload['status']).toBe(200);
    expect(payload['method']).toBe('GET');
    expect(payload['routePath']).toBe('/react/chunk.panpew1h.js');
    expect(payload['outcome']).toBe('success');
    expect(typeof payload['durationMs']).toBe('number');
    // Closed schema: durationMs replaces the old `duration` key.
    expect(payload).not.toHaveProperty('duration');
    expect(entry?.level).toBe('debug');
    expect(entry?.message).toBe('[GET] /react/chunk.panpew1h.js');
    expect(entry?.data).toBeUndefined();
  });

  specTest(
    'should log 404 with failure-free outcome when no response is returned',
    {
      feature: 'typescript/http-services',
      requirement: 'request-log-and-trace',
      check: 'a-client-outcome-logs-as-success',
    },
    async () => {
      const logger = await runWithLogger(
        {
          method: 'GET',
          route: '/missing',
          path: () => '/missing',
        },
        async () => undefined,
      );

      expect(logger.entries).toHaveLength(1);
      const payload = logger.entries[0]?.context?.['http'] as Record<string, unknown>;
      expect(payload['status']).toBe(404);
      // 404 < 500 ⇒ success outcome (a handled client outcome, not a failure).
      expect(payload['outcome']).toBe('success');
      expect(typeof payload['durationMs']).toBe('number');
    },
  );

  describe('on a pipeline request context', () => {
    afterEach(() => resetLoggerConfig());

    let current: HttpRequestContextInternal | undefined;
    /** The request context the running test serves. */
    const ctx = (): HttpRequestContextInternal => current as HttpRequestContextInternal;

    /** Runs the middleware on a real request context whose logger checks the level. */
    async function serve(
      next: () => Promise<HttpResponse | undefined>,
      init: Partial<HttpRequestContextInternal> = {},
    ) {
      const sink = new MemorySink();
      const { method, ...rest } = init;
      const ctx = buildHttpContext<HttpRequestContextInternal>({
        req: new Request('http://localhost/items/42', { method: method ?? 'GET' }),
      });
      current = ctx;
      Object.assign(ctx, { route: '/items/[id]', logger: new Logger([sink]) }, rest);
      await runInContext(ctx, () => LoggerMiddleware()(ctx, next));
      // The built log context, read without building it.
      const built = (ctx as unknown as { _logContext?: Record<string, unknown> })._logContext;
      return { entries: sink.entries, built };
    }

    specTest(
      'a request below debug writes no record and builds no http log context',
      {
        feature: 'typescript/http-services',
        requirement: 'request-log-and-trace',
        check: 'a-request-below-debug-builds-no-log-context',
      },
      async () => {
        setLogLevel('info');
        const records: string[] = [];
        const stop = observeLogRecords((level, outcome) => records.push(`${level}:${outcome}`));
        try {
          const { entries, built } = await serve(async () => HttpResponse.json({ ok: true }));
          expect(entries).toHaveLength(0);
          expect(built).toBeUndefined();
          // The dropped access record is still counted.
          expect(records).toEqual(['debug:dropped']);
        } finally {
          stop();
        }
      },
    );

    it('a record written during the request carries the http field', async () => {
      setLogLevel('info');
      const { entries, built } = await serve(async () => {
        useLogger('database').warn('query failed');
        return HttpResponse.json({ ok: true });
      });
      expect(entries).toHaveLength(1);
      expect(entries[0]?.context?.['http']).toEqual({ method: 'GET', routePath: '/items/[id]' });
      expect(built).toEqual({ http: { method: 'GET', routePath: '/items/[id]' } });
    });

    it('at debug, the access record carries the whole http field', async () => {
      setLogLevel('debug');
      const { entries } = await serve(async () => HttpResponse.json({ ok: true }));
      expect(entries).toHaveLength(1);
      expect(entries[0]?.level).toBe('debug');
      expect(entries[0]?.message).toBe('[GET] /items/[id]');
      const payload = entries[0]?.context?.['http'] as Record<string, unknown>;
      expect(payload).toMatchObject({ method: 'GET', routePath: '/items/[id]', status: 200, outcome: 'success' });
      expect(typeof payload['durationMs']).toBe('number');
    });

    it('below debug, a server error still logs at error with the whole http field', async () => {
      setLogLevel('info');
      const error = new Error('boom');
      const { entries } = await serve(async () => new HttpResponse('error', { status: 500 }), {
        __requestError: error,
      });
      expect(entries).toHaveLength(1);
      expect(entries[0]?.level).toBe('error');
      expect(entries[0]?.error?.message).toBe('boom');
      expect(entries[0]?.context?.['http']).toMatchObject({
        method: 'GET',
        routePath: '/items/[id]',
        status: 500,
        outcome: 'failure',
      });
    });

    it('the http field keeps the method the request arrived with', async () => {
      setLogLevel('debug');
      const { entries } = await serve(
        async () => {
          // HEAD served by a GET handler: the methods middleware sets the method to GET.
          (ctx() as { method: string }).method = 'GET';
          useLogger().warn('inside');
          return new HttpResponse(undefined);
        },
        { method: 'HEAD' } as Partial<HttpRequestContextInternal>,
      );
      expect(entries.map((e) => (e.context as { http: { method: string } }).http.method)).toEqual(['HEAD', 'HEAD']);
    });

    it('a context derived from the request writes the request log context', async () => {
      setLogLevel('info');
      const { entries, built } = await serve(async () => {
        // A client call's telemetry scope runs in a context derived from the request's.
        await runInContext(Object.create(ctx()), async () => {
          useLogger().with('upstream', 'items');
        });
        useLogger().warn('after');
        return HttpResponse.json({ ok: true });
      });
      expect(built).toEqual({ http: { method: 'GET', routePath: '/items/[id]' }, upstream: 'items' });
      expect(entries[0]?.context).toEqual({ http: { method: 'GET', routePath: '/items/[id]' }, upstream: 'items' });
    });

    it('a field set before the middleware keeps the http field eager', async () => {
      setLogLevel('info');
      const { entries, built } = await serve(
        async () => {
          useLogger().warn('late');
          return HttpResponse.json({ ok: true });
        },
        { logContext: { tenant: 'acme' } },
      );
      expect(built).toEqual({ tenant: 'acme', http: { method: 'GET', routePath: '/items/[id]' } });
      expect(entries[0]?.context).toEqual({ tenant: 'acme', http: { method: 'GET', routePath: '/items/[id]' } });
    });
  });
});
