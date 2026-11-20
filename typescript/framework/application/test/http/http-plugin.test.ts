import { afterEach, beforeEach, describe, expect, it } from 'bun:test';
import { NotFoundException, resetDefaultLogger, setRootLogger } from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import { MemoryLogger } from '@putnami/runtime/testing';
import type { Application } from '../../src/application';
import { application } from '../../src/application';
import { HttpPlugin, http, resolveIdleTimeoutSeconds } from '../../src/http/http.plugin';
import { logger } from '../../src/http/logger.plugin';
import { HttpResponse } from '../../src/http/http-response';

describe('HttpPlugin', () => {
  describe('Route Registration', () => {
    let plugin: HttpPlugin;

    beforeEach(() => {
      plugin = new HttpPlugin();
    });

    it('should register GET route', () => {
      plugin.get('/test', () => new HttpResponse('ok'));
      // Verify route was added to router
      expect(plugin['router']).toBeDefined();
    });

    it('should chain route methods', () => {
      const result = plugin
        .get('/a', () => new HttpResponse('a'))
        .post('/b', () => new HttpResponse('b'))
        .put('/c', () => new HttpResponse('c'));
      expect(result).toBe(plugin);
    });
  });

  describe('Merge', () => {
    it('should merge routes from another HttpPlugin', () => {
      const plugin1 = new HttpPlugin();
      const plugin2 = new HttpPlugin();

      plugin1.get('/a', () => new HttpResponse('a'));
      plugin2.get('/b', () => new HttpResponse('b'));

      const result = plugin1.merge(plugin2);
      expect(result).toBe(plugin1);
    });
  });

  describe('http() factory', () => {
    it('should create HttpPlugin instance', () => {
      const plugin = http();
      expect(plugin).toBeInstanceOf(HttpPlugin);
    });

    it('should accept options', () => {
      const plugin = http({ port: 4000 });
      expect(plugin).toBeInstanceOf(HttpPlugin);
    });
  });

  describe('Server Lifecycle', () => {
    let app: Application;
    let plugin: HttpPlugin;

    beforeEach(() => {
      plugin = http({ port: 0 }); // Port 0 = random available port
      app = application().use(plugin);
    });

    afterEach(async () => {
      await app.stop();
    });

    it('should start server on start()', async () => {
      expect(plugin.getServer()).toBeUndefined();
      await app.start();
      expect(plugin.getServer()).toBeDefined();
      expect(plugin.getServer()?.port).toBeGreaterThan(0);
    });

    it('should stop server on stop()', async () => {
      await app.start();
      const server = plugin.getServer();
      expect(server).toBeDefined();
      await app.stop();
    });

    it('should return undefined before start', () => {
      expect(plugin.getServer()).toBeUndefined();
    });

    specTest(
      'drains in-flight requests on stop() instead of aborting them',
      {
        feature: 'typescript/http-services',
        requirement: 'request-bounds',
        check: 'stop-lets-an-in-flight-request-finish',
      },
      async () => {
        plugin.get('/slow', async () => {
          await new Promise((resolve) => setTimeout(resolve, 150));
          return new HttpResponse('drained');
        });
        await app.start();
        const baseUrl = `http://localhost:${plugin.getServer()?.port}`;

        // Fire a request and let it reach the handler (in-flight) before shutdown.
        const inflight = fetch(`${baseUrl}/slow`);
        await new Promise((resolve) => setTimeout(resolve, 30));

        // Graceful drain must let the in-flight request finish, not abort it.
        await app.stop();

        const res = await inflight;
        expect(res.status).toBe(200);
        expect(await res.text()).toBe('drained');
      },
    );
  });

  describe('HTTP Requests', () => {
    let app: Application;
    let plugin: HttpPlugin;
    let baseUrl: string;

    beforeEach(async () => {
      plugin = http({ port: 0 });
      plugin.get('/text', () => new HttpResponse('plain text'));
      plugin.get(
        '/json',
        () =>
          new HttpResponse(JSON.stringify({ message: 'hello' }), { headers: { 'Content-Type': 'application/json' } }),
      );
      plugin.get('/users/[id]', (ctx) => new HttpResponse(`User: ${ctx.params?.['id']}`));
      plugin.post('/echo', async (ctx) => {
        const data = await ctx.body<{ name: string }>();
        return new HttpResponse(JSON.stringify({ received: data }), {
          headers: { 'Content-Type': 'application/json' },
        });
      });

      app = application().use(plugin);
      await app.start();
      baseUrl = `http://localhost:${plugin.getServer()?.port}`;
    });

    afterEach(async () => {
      await app.stop();
    });

    it('should respond to GET with text', async () => {
      const res = await fetch(`${baseUrl}/text`);
      expect(res.status).toBe(200);
      const text = await res.text();
      expect(text).toBe('plain text');
    });

    it('should respond to GET with JSON', async () => {
      const res = await fetch(`${baseUrl}/json`);
      expect(res.status).toBe(200);
      const json = await res.json();
      expect(json.message).toBe('hello');
    });

    it('should extract path params', async () => {
      const res = await fetch(`${baseUrl}/users/42`);
      expect(res.status).toBe(200);
      const text = await res.text();
      expect(text).toBe('User: 42');
    });

    it('should handle POST with body', async () => {
      const res = await fetch(`${baseUrl}/echo`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name: 'test' }),
      });
      expect(res.status).toBe(200);
      const json = await res.json();
      expect(json.received).toEqual({ name: 'test' });
    });

    specTest(
      'should return 404 for unknown routes',
      {
        feature: 'typescript/http-services',
        requirement: 'method-fallbacks',
        check: 'an-unknown-path-answers-404',
      },
      async () => {
        const res = await fetch(`${baseUrl}/not-found`);
        expect(res.status).toBe(404);
        expect(res.headers.get('content-type')).toContain('application/json');
        const body = await res.json();
        expect(body.error).toBe('Not Found');
        expect(body.statusCode).toBe(404);
      },
    );

    specTest(
      'should return 405 for known path with unsupported method',
      {
        feature: 'typescript/http-services',
        requirement: 'method-fallbacks',
        check: 'an-unrouted-method-answers-405-with-the-allow-header',
      },
      async () => {
        const res = await fetch(`${baseUrl}/text`, { method: 'POST' });
        expect(res.status).toBe(405);
        expect(res.headers.get('allow')).toContain('GET');
        expect(res.headers.get('allow')).toContain('HEAD');
        expect(res.headers.get('allow')).toContain('OPTIONS');
        const body = await res.json();
        expect(body.error).toBe('Method Not Allowed');
        expect(body.statusCode).toBe(405);
      },
    );
  });

  describe('Secure-by-default headers', () => {
    let app: Application;

    afterEach(async () => {
      await app.stop();
    });

    const startServer = async (plugin: HttpPlugin): Promise<string> => {
      plugin.get('/text', () => new HttpResponse('plain text'));
      app = application().use(plugin);
      await app.start();
      return `http://localhost:${plugin.getServer()?.port}`;
    };

    specTest(
      'applies hardened security headers without an extra plugin',
      {
        feature: 'typescript/http-services',
        requirement: 'secure-by-default',
        check: 'security-headers-apply-without-an-extra-plugin',
      },
      async () => {
        const baseUrl = await startServer(http({ port: 0 }));
        const res = await fetch(`${baseUrl}/text`);

        expect(res.headers.get('x-content-type-options')).toBe('nosniff');
        expect(res.headers.get('x-frame-options')).toBe('DENY');
        expect(res.headers.get('content-security-policy')).toContain("default-src 'self'");
        expect(res.headers.get('referrer-policy')).toBe('strict-origin-when-cross-origin');
      },
    );

    it('omits security headers when securityHeaders is false', async () => {
      const baseUrl = await startServer(http({ port: 0, securityHeaders: false }));
      const res = await fetch(`${baseUrl}/text`);

      expect(res.headers.get('x-content-type-options')).toBeNull();
      expect(res.headers.get('x-frame-options')).toBeNull();
      expect(res.headers.get('content-security-policy')).toBeNull();
    });

    it('honours per-header overrides', async () => {
      const baseUrl = await startServer(
        http({ port: 0, securityHeaders: { frameOptions: 'SAMEORIGIN', contentSecurityPolicy: false } }),
      );
      const res = await fetch(`${baseUrl}/text`);

      expect(res.headers.get('x-frame-options')).toBe('SAMEORIGIN');
      expect(res.headers.get('content-security-policy')).toBeNull();
      // Other defaults remain in place.
      expect(res.headers.get('x-content-type-options')).toBe('nosniff');
    });

    specTest(
      'applies security headers to origin-guard rejection responses',
      {
        feature: 'typescript/http-services',
        requirement: 'secure-by-default',
        check: 'a-refusal-carries-the-security-headers',
      },
      async () => {
        const plugin = http({ port: 0 });
        plugin.post('/mutate', () => HttpResponse.json({ ok: true }));
        app = application().use(plugin);
        await app.start();
        const baseUrl = `http://localhost:${plugin.getServer()?.port}`;

        // Cross-origin state-changing request is rejected by the origin guard (403),
        // and the security headers still wrap that response.
        const res = await fetch(`${baseUrl}/mutate`, {
          method: 'POST',
          headers: { Origin: 'https://evil.example.com' },
        });

        expect(res.status).toBe(403);
        expect(res.headers.get('x-content-type-options')).toBe('nosniff');
        expect(res.headers.get('x-frame-options')).toBe('DENY');
      },
    );
  });

  describe('Handler Auto-Wrap', () => {
    let app: Application;
    let plugin: HttpPlugin;
    let baseUrl: string;

    beforeEach(async () => {
      plugin = http({ port: 0 });
      // Return plain object - should be auto-wrapped as JSON
      plugin.get('/auto-json', () => ({ data: 'hello', count: 42 }));
      // Return plain string - should be auto-wrapped as text
      plugin.get('/auto-text', () => 'plain string response');
      // Return undefined - should pass to 404
      plugin.get('/auto-undefined', () => undefined);

      app = application().use(plugin);
      await app.start();
      baseUrl = `http://localhost:${plugin.getServer()?.port}`;
    });

    afterEach(async () => {
      await app.stop();
    });

    it('should auto-wrap object as JSON', async () => {
      const res = await fetch(`${baseUrl}/auto-json`);
      expect(res.status).toBe(200);
      expect(res.headers.get('content-type')).toBe('application/json');
      const json = await res.json();
      expect(json).toEqual({ data: 'hello', count: 42 });
    });

    it('should auto-wrap string as text', async () => {
      const res = await fetch(`${baseUrl}/auto-text`);
      expect(res.status).toBe(200);
      const text = await res.text();
      expect(text).toBe('plain string response');
    });

    it('should treat undefined as no response (404)', async () => {
      const res = await fetch(`${baseUrl}/auto-undefined`);
      expect(res.status).toBe(404);
    });
  });

  describe('Request Safeguards', () => {
    let app: Application;
    let plugin: HttpPlugin;
    let baseUrl: string;
    let stalled: { signal?: AbortSignal; deadlineAt?: number; context?: Record<string, unknown> } | undefined;
    let workAfterTimeout = false;
    let reported: unknown[] = [];

    beforeEach(async () => {
      stalled = undefined;
      workAfterTimeout = false;
      reported = [];
      plugin = http({
        port: 0,
        maxBodySizeBytes: 64,
        requestTimeoutMs: 200,
        onError: (error) => {
          reported.push(error);
          return undefined;
        },
      });
      plugin.post('/echo', async (ctx) => {
        const data = await ctx.body<Record<string, string>>();
        return HttpResponse.json({ received: data });
      });
      plugin.get('/slow', async () => {
        await new Promise((resolve) => setTimeout(resolve, 500));
        return new HttpResponse('slow-ok');
      });
      plugin.get('/stalled', async (ctx) => {
        stalled = {
          signal: ctx.signal,
          deadlineAt: ctx.deadlineAt as number | undefined,
          context: ctx as unknown as Record<string, unknown>,
        };
        // Waits on a dependency that never answers; only the signal ends the wait.
        await new Promise((_, reject) => ctx.signal?.addEventListener('abort', () => reject(ctx.signal?.reason)));
        workAfterTimeout = true;
        return new HttpResponse('never');
      });
      plugin.get('/broken-after-timeout', async (ctx) => {
        stalled = { signal: ctx.signal, context: ctx as unknown as Record<string, unknown> };
        // Ignores the signal, then fails for a reason of its own once the request ended.
        await new Promise((resolve) => setTimeout(resolve, 300));
        throw new TypeError('the handler broke');
      });

      plugin.get('/client-deadline', (ctx) => {
        // A client call bound by the request deadline stops at that deadline,
        // a moment before the timeout aborts the request's signal.
        const remaining = (ctx.deadlineAt ?? Date.now()) - Date.now();
        return new Promise((_, reject) =>
          setTimeout(
            () => reject(Object.assign(new Error('deadline'), { name: 'ClientDeadlineError' })),
            Math.max(0, remaining),
          ),
        );
      });
      plugin.get('/missing-after-timeout', async () => {
        await new Promise((resolve) => setTimeout(resolve, 300));
        throw new NotFoundException('gone');
      });

      app = application().use(plugin).use(logger());
      await app.start();
      baseUrl = `http://localhost:${plugin.getServer()?.port}`;
    });

    afterEach(async () => {
      await app.stop();
    });

    specTest(
      'should reject requests bigger than maxBodySizeBytes',
      {
        feature: 'typescript/http-services',
        requirement: 'request-bounds',
        check: 'an-oversized-body-answers-413',
      },
      async () => {
        const payload = JSON.stringify({ text: 'x'.repeat(512) });
        const res = await fetch(`${baseUrl}/echo`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: payload,
        });

        expect(res.status).toBe(413);
      },
    );

    specTest(
      'should timeout long-running requests',
      {
        feature: 'typescript/http-services',
        requirement: 'request-bounds',
        check: 'a-request-past-the-timeout-answers-408',
      },
      async () => {
        const res = await fetch(`${baseUrl}/slow`);
        expect(res.status).toBe(408);
        const body = await res.json();
        expect(body.error).toBe('Request Timeout');
        expect(body.statusCode).toBe(408);
      },
    );

    specTest(
      'aborts the signal of a request past its timeout',
      {
        feature: 'typescript/http-services',
        requirement: 'request-timeout-cancels-work',
        check: 'a-request-past-the-timeout-aborts-its-signal',
      },
      async () => {
        const sent = Date.now();
        const res = await fetch(`${baseUrl}/stalled`);
        expect(res.status).toBe(408);
        expect(stalled?.signal?.aborted).toBe(true);
        const reason = stalled?.signal?.reason as Error | undefined;
        expect(reason?.name).toBe('TimeoutError');
        expect(stalled?.deadlineAt).toBeGreaterThanOrEqual(sent + 200);
        expect(stalled?.deadlineAt).toBeLessThan(sent + 1000);
        await new Promise((resolve) => setTimeout(resolve, 20));
        expect(workAfterTimeout).toBe(false);
      },
    );

    specTest(
      'a handler that stops on its ended request logs and records no error',
      {
        feature: 'typescript/http-services',
        requirement: 'request-timeout-cancels-work',
        check: 'an-ended-request-logs-no-error',
      },
      async () => {
        const memory = new MemoryLogger();
        setRootLogger(memory);
        try {
          const res = await fetch(`${baseUrl}/stalled`);
          expect(res.status).toBe(408);
          await new Promise((resolve) => setTimeout(resolve, 50));
          expect(memory.entries.filter((entry) => entry.level === 'error')).toEqual([]);
          const access = memory.entries.find((entry) => entry.message.includes('[GET] /stalled'));
          expect(access?.level).toBe('debug');
          expect(stalled?.context?.['__requestError']).toBeUndefined();
          expect(reported).toEqual([]);
        } finally {
          resetDefaultLogger();
        }
      },
    );

    specTest(
      'a handler that fails for its own reason after its request ended is still logged',
      {
        feature: 'typescript/http-services',
        requirement: 'request-timeout-cancels-work',
        check: 'a-failure-after-the-request-ended-is-still-logged',
      },
      async () => {
        const memory = new MemoryLogger();
        setRootLogger(memory);
        try {
          const res = await fetch(`${baseUrl}/broken-after-timeout`);
          expect(res.status).toBe(408);
          await new Promise((resolve) => setTimeout(resolve, 200));
          const errors = memory.entries.filter((entry) => entry.level === 'error');
          expect(errors.some((entry) => JSON.stringify(entry).includes('the handler broke'))).toBe(true);
          expect(reported).toHaveLength(1);
          expect((reported[0] as Error).name).toBe('TypeError');
          expect((stalled?.context?.['__requestError'] as Error | undefined)?.name).toBe('TypeError');
        } finally {
          resetDefaultLogger();
        }
      },
    );

    specTest(
      'a client call stopped at the request deadline answers 408 and logs nothing',
      {
        feature: 'typescript/http-services',
        requirement: 'request-timeout-cancels-work',
        check: 'a-client-call-at-the-deadline-answers-408',
      },
      async () => {
        const memory = new MemoryLogger();
        setRootLogger(memory);
        try {
          // The client's timer and the request's timer race: every run must answer 408.
          const statuses = await Promise.all(
            Array.from({ length: 10 }, async () => (await fetch(`${baseUrl}/client-deadline`)).status),
          );
          expect(statuses).toEqual(Array.from({ length: 10 }, () => 408));
          await new Promise((resolve) => setTimeout(resolve, 50));
          expect(memory.entries.filter((entry) => entry.level === 'error')).toEqual([]);
          expect(reported).toEqual([]);
        } finally {
          resetDefaultLogger();
        }
      },
    );

    it('logs after the request ended only what it logs during the request', async () => {
      const memory = new MemoryLogger();
      setRootLogger(memory);
      try {
        const res = await fetch(`${baseUrl}/missing-after-timeout`);
        expect(res.status).toBe(408);
        await new Promise((resolve) => setTimeout(resolve, 200));
        expect(memory.entries.filter((entry) => entry.level === 'error')).toEqual([]);
        expect(reported).toHaveLength(1);
      } finally {
        resetDefaultLogger();
      }
    });
  });

  describe('Application Integration', () => {
    it('should work with ensurePlugin', async () => {
      const app = application();
      const plugin = await app.ensurePlugin(HttpPlugin);
      expect(plugin).toBeInstanceOf(HttpPlugin);
    });

    it('should be accessible after app.start()', async () => {
      const app = application().use(http({ port: 0 }));
      await app.start();
      const plugin = await app.getPlugin(HttpPlugin);
      expect(plugin.getServer()).toBeDefined();
      await app.stop();
    });
  });

  describe('Redirect behavior', () => {
    let app: Application;
    let plugin: HttpPlugin;
    let baseUrl: string;

    beforeEach(async () => {
      plugin = http({ port: 0 });
      plugin.get('/moved', () => HttpResponse.redirect('/new-home', 301));
      app = application().use(plugin);
      await app.start();
      baseUrl = `http://localhost:${plugin.getServer()?.port}`;
    });

    afterEach(async () => {
      await app.stop();
    });

    it('should preserve redirect status code', async () => {
      const res = await fetch(`${baseUrl}/moved`, { redirect: 'manual' });
      expect(res.status).toBe(301);
      expect(res.headers.get('location')).toBe('/new-home');
    });
  });
});

describe('resolveIdleTimeoutSeconds', () => {
  specTest(
    'derives an explicit idle timeout (seconds) from the request timeout, with buffer',
    {
      feature: 'typescript/http-services',
      requirement: 'request-bounds',
      check: 'the-idle-timeout-follows-the-request-timeout-within-bun-bounds',
    },
    () => {
      expect(resolveIdleTimeoutSeconds(30_000)).toBe(35);
      expect(resolveIdleTimeoutSeconds(10_000)).toBe(15);
    },
  );

  specTest(
    'falls back to a sane default when the request timeout is disabled',
    {
      feature: 'typescript/http-services',
      requirement: 'request-bounds',
      check: 'the-idle-timeout-follows-the-request-timeout-within-bun-bounds',
    },
    () => {
      expect(resolveIdleTimeoutSeconds(0)).toBe(35);
    },
  );

  specTest(
    'clamps to Bun supported range [1, 255]',
    {
      feature: 'typescript/http-services',
      requirement: 'request-bounds',
      check: 'the-idle-timeout-follows-the-request-timeout-within-bun-bounds',
    },
    () => {
      expect(resolveIdleTimeoutSeconds(10_000_000)).toBe(255);
      expect(resolveIdleTimeoutSeconds(1)).toBeGreaterThanOrEqual(1);
    },
  );
});
