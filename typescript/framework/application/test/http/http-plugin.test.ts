import { afterEach, beforeEach, describe, expect, it } from 'bun:test';
import type { Application } from '../../src/application';
import { application } from '../../src/application';
import { HttpPlugin, http, resolveIdleTimeoutSeconds } from '../../src/http/http.plugin';
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
      expect(plugin.router).toBeDefined();
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

    it('drains in-flight requests on stop() instead of aborting them', async () => {
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
    });
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
      plugin.get('/users/[id]', (ctx) => new HttpResponse(`User: ${ctx.params?.id}`));
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

    it('should return 404 for unknown routes', async () => {
      const res = await fetch(`${baseUrl}/not-found`);
      expect(res.status).toBe(404);
      expect(res.headers.get('content-type')).toContain('application/json');
      const body = await res.json();
      expect(body.error).toBe('Not Found');
      expect(body.statusCode).toBe(404);
    });

    it('should return 405 for known path with unsupported method', async () => {
      const res = await fetch(`${baseUrl}/text`, { method: 'POST' });
      expect(res.status).toBe(405);
      expect(res.headers.get('allow')).toContain('GET');
      expect(res.headers.get('allow')).toContain('HEAD');
      expect(res.headers.get('allow')).toContain('OPTIONS');
      const body = await res.json();
      expect(body.error).toBe('Method Not Allowed');
      expect(body.statusCode).toBe(405);
    });
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

    it('applies hardened security headers without an extra plugin', async () => {
      const baseUrl = await startServer(http({ port: 0 }));
      const res = await fetch(`${baseUrl}/text`);

      expect(res.headers.get('x-content-type-options')).toBe('nosniff');
      expect(res.headers.get('x-frame-options')).toBe('DENY');
      expect(res.headers.get('content-security-policy')).toContain("default-src 'self'");
      expect(res.headers.get('referrer-policy')).toBe('strict-origin-when-cross-origin');
    });

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

    it('applies security headers to origin-guard rejection responses', async () => {
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
    });
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

    beforeEach(async () => {
      plugin = http({ port: 0, maxBodySizeBytes: 64, requestTimeoutMs: 200 });
      plugin.post('/echo', async (ctx) => {
        const data = await ctx.body<Record<string, string>>();
        return HttpResponse.json({ received: data });
      });
      plugin.get('/slow', async () => {
        await new Promise((resolve) => setTimeout(resolve, 500));
        return new HttpResponse('slow-ok');
      });

      app = application().use(plugin);
      await app.start();
      baseUrl = `http://localhost:${plugin.getServer()?.port}`;
    });

    afterEach(async () => {
      await app.stop();
    });

    it('should reject requests bigger than maxBodySizeBytes', async () => {
      const payload = JSON.stringify({ text: 'x'.repeat(512) });
      const res = await fetch(`${baseUrl}/echo`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: payload,
      });

      expect(res.status).toBe(413);
    });

    it('should timeout long-running requests', async () => {
      const res = await fetch(`${baseUrl}/slow`);
      expect(res.status).toBe(408);
      const body = await res.json();
      expect(body.error).toBe('Request Timeout');
      expect(body.statusCode).toBe(408);
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
  it('derives an explicit idle timeout (seconds) from the request timeout, with buffer', () => {
    expect(resolveIdleTimeoutSeconds(30_000)).toBe(35);
    expect(resolveIdleTimeoutSeconds(10_000)).toBe(15);
  });

  it('falls back to a sane default when the request timeout is disabled', () => {
    expect(resolveIdleTimeoutSeconds(0)).toBe(35);
  });

  it('clamps to Bun supported range [1, 255]', () => {
    expect(resolveIdleTimeoutSeconds(10_000_000)).toBe(255);
    expect(resolveIdleTimeoutSeconds(1)).toBeGreaterThanOrEqual(1);
  });
});
