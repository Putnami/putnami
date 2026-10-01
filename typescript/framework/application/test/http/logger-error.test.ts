import { describe, expect, it } from 'bun:test';
import { HttpException, runInContext } from '@putnami/runtime';
import { MemoryLogger } from '@putnami/runtime/testing';
import { HttpResponse } from '../../src/http/http-response';
import { LoggerMiddleware } from '../../src/http/logger.middleware';

async function runWithLogger(
  context: Record<string, unknown>,
  next: () => Promise<HttpResponse | undefined>,
  options?: Parameters<typeof LoggerMiddleware>[0],
): Promise<MemoryLogger> {
  const logger = new MemoryLogger();
  const runtimeContext = { ...context, logger };
  const middleware = LoggerMiddleware(options);
  await runInContext(runtimeContext, () => middleware(runtimeContext as never, next));
  return logger;
}

describe('LoggerMiddleware - error and edge cases', () => {
  it('should log 500 error with request error context', async () => {
    const requestError = new Error('Database connection failed');
    const logger = await runWithLogger(
      {
        method: 'POST',
        route: '/api/data',
        path: () => '/api/data',
        __requestError: requestError,
      },
      async () => new HttpResponse('error', { status: 500 }),
    );

    expect(logger.entries).toHaveLength(1);
    const entry = logger.entries[0];
    const payload = entry?.context?.['http'] as Record<string, unknown>;
    const message = entry?.message || '';
    // Message is `[METHOD] routePath` on every path — no error text leaks in.
    expect(message).toBe('[POST] /api/data');
    expect(message).not.toContain('Database connection failed');
    // The real error rides on entry.error (the log param), not the message.
    expect(entry?.error?.message).toBe(requestError.message);
    expect(entry?.level).toBe('error');
    expect(payload['status']).toBe(500);
    expect(payload['outcome']).toBe('failure');
    expect(typeof payload['durationMs']).toBe('number');
  });

  it('should log non-Error request error without leaking text into the message', async () => {
    const logger = await runWithLogger(
      {
        method: 'POST',
        route: '/api/data',
        path: () => '/api/data',
        __requestError: 'string error',
      },
      async () => new HttpResponse('error', { status: 500 }),
    );

    expect(logger.entries).toHaveLength(1);
    const entry = logger.entries[0];
    const payload = entry?.context?.['http'] as Record<string, unknown>;
    expect(entry?.message).toBe('[POST] /api/data');
    expect(entry?.message).not.toContain('Internal Server Error');
    expect(payload['status']).toBe(500);
    expect(payload['outcome']).toBe('failure');
  });

  it('should catch and re-throw errors from handler', async () => {
    const thrownError = new Error('handler crashed');
    const logger = new MemoryLogger();
    const context = {
      logger,
      method: 'GET',
      route: '/api/crash',
      path: () => '/api/crash',
      __requestErrorLogged: false,
    };
    const middleware = LoggerMiddleware();

    await expect(
      runInContext(context, () =>
        middleware(context as never, async () => {
          throw thrownError;
        }),
      ),
    ).rejects.toThrow('handler crashed');

    expect(logger.entries).toHaveLength(1);
    const entry = logger.entries[0];
    const payload = entry?.context?.['http'] as Record<string, unknown>;
    expect(entry?.message).toBe('[GET] /api/crash');
    expect(entry?.message).not.toContain('handler crashed');
    expect(entry?.error?.message).toBe('handler crashed');
    expect(payload['status']).toBe(500);
    expect(payload['outcome']).toBe('failure');
    expect(typeof payload['durationMs']).toBe('number');
    expect(context.__requestErrorLogged).toBe(true);
  });

  it('should log HttpException with correct status', async () => {
    const httpError = new HttpException('Forbidden resource', 403);
    const logger = new MemoryLogger();
    const context = {
      logger,
      method: 'GET',
      route: '/admin',
      path: () => '/admin',
    };
    const middleware = LoggerMiddleware();

    await expect(
      runInContext(context, () =>
        middleware(context as never, async () => {
          throw httpError;
        }),
      ),
    ).rejects.toThrow();

    expect(logger.entries).toHaveLength(1);
    const entry = logger.entries[0];
    const payload = entry?.context?.['http'] as Record<string, unknown>;
    expect(entry?.message).toBe('[GET] /admin');
    expect(payload['status']).toBe(403);
    // 403 < 500 ⇒ success outcome even though it surfaced as an exception.
    expect(payload['outcome']).toBe('success');
  });

  it('should skip logging for excluded routes', async () => {
    const logger = await runWithLogger(
      {
        method: 'GET',
        route: '/_/health',
        path: () => '/_/health',
      },
      async () => new HttpResponse('ok'),
      { exclude: ['/_/health'] },
    );

    expect(logger.entries).toHaveLength(0);
  });

  it('should skip logging when path matches exclusion', async () => {
    const logger = await runWithLogger(
      {
        method: 'GET',
        route: undefined,
        path: () => '/health',
      },
      async () => new HttpResponse('ok'),
      { exclude: ['/health'] },
    );

    expect(logger.entries).toHaveLength(0);
  });

  it('should prepend / to route if missing', async () => {
    const logger = await runWithLogger(
      {
        method: 'GET',
        route: 'api/test',
        path: () => 'api/test',
      },
      async () => new HttpResponse('ok'),
    );

    expect(logger.entries).toHaveLength(1);
    const message = logger.entries[0]?.message || '';
    expect(message).toContain('/api/test');
  });

  it('should use path() when route is undefined', async () => {
    const logger = await runWithLogger(
      {
        method: 'GET',
        route: undefined,
        path: () => '/fallback-path',
      },
      async () => new HttpResponse('ok'),
    );

    expect(logger.entries).toHaveLength(1);
    const message = logger.entries[0]?.message || '';
    expect(message).toContain('/fallback-path');
  });
});
