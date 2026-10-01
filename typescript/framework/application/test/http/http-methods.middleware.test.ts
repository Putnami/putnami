import { afterEach, describe, expect, it } from 'bun:test';
import type { Application } from '../../src/application';
import { application } from '../../src/application';
import { CorsMiddleware } from '../../src/http/cors.middleware';
import { buildTraceResponse } from '../../src/http/http-methods.middleware';
import { http } from '../../src/http/http.plugin';
import type { HttpPlugin } from '../../src/http/http.plugin';
import { HttpResponse } from '../../src/http/http-response';

describe('HttpMethodsMiddleware', () => {
  let app: Application;
  let plugin: HttpPlugin;
  let baseUrl: string;

  afterEach(async () => {
    await app.stop();
  });

  const start = async (p: HttpPlugin) => {
    plugin = p;
    app = application().use(plugin);
    await app.start();
    baseUrl = `http://localhost:${plugin.getServer()?.port}`;
  };

  describe('HEAD', () => {
    it('should derive HEAD response from GET handler', async () => {
      const p = http({ port: 0 });
      p.get('/hello', () => 'Hello, World!');
      await start(p);

      const res = await fetch(`${baseUrl}/hello`, { method: 'HEAD' });
      expect(res.status).toBe(200);

      const body = await res.text();
      expect(body).toBe('');
    });

    it('should preserve headers from GET handler', async () => {
      const p = http({ port: 0 });
      p.get('/json', () => HttpResponse.json({ key: 'value' }));
      await start(p);

      const res = await fetch(`${baseUrl}/json`, { method: 'HEAD' });
      expect(res.status).toBe(200);
      expect(res.headers.get('content-type')).toBe('application/json');

      const body = await res.text();
      expect(body).toBe('');
    });

    it('should execute the handler to compute response metadata', async () => {
      let handlerCalled = false;
      const p = http({ port: 0 });
      p.get('/computed', () => {
        handlerCalled = true;
        return HttpResponse.json({ data: [1, 2, 3] });
      });
      await start(p);

      await fetch(`${baseUrl}/computed`, { method: 'HEAD' });
      expect(handlerCalled).toBe(true);
    });

    it('should return 404 for HEAD on non-existent route', async () => {
      const p = http({ port: 0 });
      p.get('/exists', () => 'ok');
      await start(p);

      const res = await fetch(`${baseUrl}/not-here`, { method: 'HEAD' });
      expect(res.status).toBe(404);
    });

    it('should not affect HEAD on POST-only route', async () => {
      const p = http({ port: 0 });
      p.post('/submit', () => HttpResponse.json({ done: true }));
      await start(p);

      const res = await fetch(`${baseUrl}/submit`, { method: 'HEAD' });
      expect(res.status).toBe(404);
    });

    it('should work with dynamic route params', async () => {
      const p = http({ port: 0 });
      p.get('/users/[id]', (ctx) => HttpResponse.json({ id: ctx.params?.id }));
      await start(p);

      const res = await fetch(`${baseUrl}/users/42`, { method: 'HEAD' });
      expect(res.status).toBe(200);
      expect(res.headers.get('content-type')).toBe('application/json');
      expect(await res.text()).toBe('');
    });

    it('should be disabled when httpMethods.head is false', async () => {
      const p = http({ port: 0, httpMethods: { head: false } });
      p.get('/hello', () => 'Hello');
      await start(p);

      const res = await fetch(`${baseUrl}/hello`, { method: 'HEAD' });
      expect(res.status).toBe(404);
    });

    it('should be disabled when httpMethods is false', async () => {
      const p = http({ port: 0, httpMethods: false });
      p.get('/hello', () => 'Hello');
      await start(p);

      const res = await fetch(`${baseUrl}/hello`, { method: 'HEAD' });
      expect(res.status).toBe(404);
    });

    it('should use headMeta when provided and skip the handler', async () => {
      let handlerCalled = false;
      const p = http({ port: 0 });
      p.route(
        'GET',
        '/static',
        () => {
          handlerCalled = true;
          return HttpResponse.json({ data: 'large' });
        },
        {
          headMeta: () =>
            new HttpResponse(undefined, {
              status: 200,
              headers: {
                'Content-Type': 'text/html',
                'Content-Length': '1234',
                'Cache-Control': 'public, max-age=86400',
                ETag: '"abc123"',
              },
            }),
        },
      );
      await start(p);

      const res = await fetch(`${baseUrl}/static`, { method: 'HEAD' });
      expect(res.status).toBe(200);
      expect(res.headers.get('content-type')).toBe('text/html');
      expect(res.headers.get('content-length')).toBe('1234');
      expect(res.headers.get('cache-control')).toBe('public, max-age=86400');
      expect(res.headers.get('etag')).toBe('"abc123"');
      expect(await res.text()).toBe('');
      expect(handlerCalled).toBe(false);
    });

    it('should fall back to handler when headMeta returns undefined', async () => {
      let handlerCalled = false;
      const p = http({ port: 0 });
      p.route(
        'GET',
        '/fallback',
        () => {
          handlerCalled = true;
          return HttpResponse.json({ ok: true });
        },
        {
          headMeta: () => undefined,
        },
      );
      await start(p);

      const res = await fetch(`${baseUrl}/fallback`, { method: 'HEAD' });
      expect(res.status).toBe(200);
      expect(res.headers.get('content-type')).toBe('application/json');
      expect(await res.text()).toBe('');
      expect(handlerCalled).toBe(true);
    });

    it('should support headMeta returning 304 for If-None-Match', async () => {
      const p = http({ port: 0 });
      p.route('GET', '/cached', () => HttpResponse.json({ data: 'value' }), {
        headMeta: (ctx) => {
          if (ctx.headers.get('If-None-Match') === '"etag-abc"') {
            return new HttpResponse(undefined, { status: 304, headers: { ETag: '"etag-abc"' } });
          }
          return new HttpResponse(undefined, {
            status: 200,
            headers: { 'Content-Type': 'application/json', ETag: '"etag-abc"' },
          });
        },
      });
      await start(p);

      const res = await fetch(`${baseUrl}/cached`, {
        method: 'HEAD',
        headers: { 'If-None-Match': '"etag-abc"' },
      });
      expect(res.status).toBe(304);
      expect(res.headers.get('etag')).toBe('"etag-abc"');
    });
  });

  describe('OPTIONS', () => {
    it('should respond with Allow header listing available methods', async () => {
      const p = http({ port: 0 });
      p.get('/items', () => 'list');
      p.post('/items', () => 'create');
      await start(p);

      const res = await fetch(`${baseUrl}/items`, { method: 'OPTIONS' });
      expect(res.status).toBe(204);

      const allow = res.headers.get('allow');
      expect(allow).toBeDefined();
      expect(allow).toContain('GET');
      expect(allow).toContain('POST');
      expect(allow).toContain('HEAD');
      expect(allow).toContain('OPTIONS');
    });

    it('should include DELETE when registered', async () => {
      const p = http({ port: 0 });
      p.get('/items/[id]', () => 'get');
      p.delete('/items/[id]', () => 'delete');
      await start(p);

      const res = await fetch(`${baseUrl}/items/1`, { method: 'OPTIONS' });
      expect(res.status).toBe(204);

      const allow = res.headers.get('allow');
      expect(allow).toContain('GET');
      expect(allow).toContain('DELETE');
      expect(allow).toContain('HEAD');
      expect(allow).toContain('OPTIONS');
    });

    it('should return 404 for OPTIONS on non-existent route', async () => {
      const p = http({ port: 0 });
      p.get('/exists', () => 'ok');
      await start(p);

      const res = await fetch(`${baseUrl}/not-here`, { method: 'OPTIONS' });
      expect(res.status).toBe(404);
    });

    it('should not include TRACE by default', async () => {
      const p = http({ port: 0 });
      p.get('/items', () => 'list');
      await start(p);

      const res = await fetch(`${baseUrl}/items`, { method: 'OPTIONS' });
      const allow = res.headers.get('allow');
      expect(allow).not.toContain('TRACE');
    });

    it('should include TRACE when enabled', async () => {
      const p = http({ port: 0, httpMethods: { trace: true } });
      p.get('/items', () => 'list');
      await start(p);

      const res = await fetch(`${baseUrl}/items`, { method: 'OPTIONS' });
      const allow = res.headers.get('allow');
      expect(allow).toContain('TRACE');
    });

    it('should be disabled when httpMethods.options is false', async () => {
      const p = http({ port: 0, httpMethods: { options: false } });
      p.get('/hello', () => 'Hello');
      await start(p);

      const res = await fetch(`${baseUrl}/hello`, { method: 'OPTIONS' });
      expect(res.status).toBe(404);
    });

    it('should cooperate with CORS middleware', async () => {
      const p = http({ port: 0 });
      p.use(CorsMiddleware());
      p.get('/data', () => HttpResponse.json({ v: 1 }));
      await start(p);

      // CORS preflight with Origin header — CORS should handle it
      const preflight = await fetch(`${baseUrl}/data`, {
        method: 'OPTIONS',
        headers: {
          Origin: 'https://example.com',
          'Access-Control-Request-Method': 'GET',
        },
      });
      expect(preflight.status).toBe(204);
      expect(preflight.headers.get('access-control-allow-origin')).toBe('*');
    });
  });

  describe('TRACE', () => {
    it('should be disabled by default', async () => {
      const p = http({ port: 0 });
      p.get('/hello', () => 'Hello');
      await start(p);

      const res = await fetch(`${baseUrl}/hello`, { method: 'TRACE' });
      expect(res.status).toBe(404);
    });

    it('should respond with 200 and message/http content-type when enabled', async () => {
      const p = http({ port: 0, httpMethods: { trace: true } });
      p.get('/echo', () => 'ok');
      await start(p);

      const res = await fetch(`${baseUrl}/echo`, {
        method: 'TRACE',
        headers: { 'X-Custom': 'test-value' },
      });
      expect(res.status).toBe(200);
      expect(res.headers.get('content-type')).toBe('message/http');
      // Note: Bun's fetch() client strips the response body for TRACE requests,
      // so we verify the body content via unit tests on buildTraceResponse below.
    });
  });

  describe('buildTraceResponse (unit)', () => {
    it('should echo the request line and headers', () => {
      const req = new Request('http://localhost/echo?q=1', {
        method: 'TRACE',
        headers: { 'X-Custom': 'test-value', Accept: '*/*' },
      });
      const ctx = {
        path: () => '/echo',
        query: () => 'q=1',
        req,
      };
      // biome-ignore lint/suspicious/noExplicitAny: test helper
      const res = buildTraceResponse(req, ctx as any);
      const native = res.get();
      expect(native.status).toBe(200);
      expect(native.headers.get('content-type')).toBe('message/http');

      const body = new Response(native.body).text();
      return body.then((text) => {
        expect(text).toContain('TRACE /echo?q=1 HTTP/1.1');
        expect(text).toContain('x-custom: test-value');
      });
    });

    it('should exclude sensitive headers', () => {
      const req = new Request('http://localhost/secure', {
        method: 'TRACE',
        headers: {
          Cookie: 'session=abc123',
          Authorization: 'Bearer secret',
          'X-Safe': 'visible',
        },
      });
      const ctx = {
        path: () => '/secure',
        query: () => '',
        req,
      };
      // biome-ignore lint/suspicious/noExplicitAny: test helper
      const res = buildTraceResponse(req, ctx as any);
      const native = res.get();

      const body = new Response(native.body).text();
      return body.then((text) => {
        expect(text).not.toContain('session=abc123');
        expect(text).not.toContain('Bearer secret');
        expect(text).toContain('x-safe: visible');
      });
    });
  });

  describe('globally disabled', () => {
    it('should disable all auto-methods when httpMethods is false', async () => {
      const p = http({ port: 0, httpMethods: false });
      p.get('/test', () => 'ok');
      p.post('/test', () => 'ok');
      await start(p);

      const head = await fetch(`${baseUrl}/test`, { method: 'HEAD' });
      expect(head.status).toBe(404);

      const options = await fetch(`${baseUrl}/test`, { method: 'OPTIONS' });
      expect(options.status).toBe(404);

      const trace = await fetch(`${baseUrl}/test`, { method: 'TRACE' });
      expect(trace.status).toBe(404);
    });
  });
});
