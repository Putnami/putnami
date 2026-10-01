import { describe, expect, it } from 'bun:test';
import { runInContext } from '@putnami/runtime';
import { MemoryLogger } from '@putnami/runtime/testing';
import { application } from '../../src/application';
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
    expect(entry?.level).toBe('info');
    expect(entry?.message).toBe('[GET] /react/chunk.panpew1h.js');
    expect(entry?.data).toBeUndefined();
  });

  it('should log 404 with failure-free outcome when no response is returned', async () => {
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
  });
});
