import { describe, expect, it } from 'bun:test';
import { http } from '../../src/http/http.plugin';
import { HttpResponse } from '../../src/http/http-response';
import { CorsMiddleware } from '../../src/http/cors.middleware';
import { application } from '../../src/application';

describe('CorsMiddleware', () => {
  describe('preflight (OPTIONS)', () => {
    it('should respond to preflight with default settings', async () => {
      const httpPlugin = http({ port: 0 });
      httpPlugin.use(CorsMiddleware());
      httpPlugin.get('/test', () => 'ok');

      const app = application().use(httpPlugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const res = await fetch(`${baseUrl}/test`, {
        method: 'OPTIONS',
        headers: { Origin: 'https://example.com' },
      });
      expect(res.status).toBe(204);
      expect(res.headers.get('Access-Control-Allow-Origin')).toBe('*');
      expect(res.headers.get('Access-Control-Allow-Methods')).toContain('GET');

      await app.stop();
    });

    it('should include requested headers in preflight', async () => {
      const httpPlugin = http({ port: 0 });
      httpPlugin.use(CorsMiddleware());
      httpPlugin.get('/test', () => 'ok');

      const app = application().use(httpPlugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const res = await fetch(`${baseUrl}/test`, {
        method: 'OPTIONS',
        headers: {
          Origin: 'https://example.com',
          'Access-Control-Request-Headers': 'Content-Type, Authorization',
        },
      });
      expect(res.headers.get('Access-Control-Allow-Headers')).toBe('Content-Type, Authorization');

      await app.stop();
    });

    it('should set max-age when configured', async () => {
      const httpPlugin = http({ port: 0 });
      httpPlugin.use(CorsMiddleware({ maxAge: 3600 }));
      httpPlugin.get('/test', () => 'ok');

      const app = application().use(httpPlugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const res = await fetch(`${baseUrl}/test`, {
        method: 'OPTIONS',
        headers: { Origin: 'https://example.com' },
      });
      expect(res.headers.get('Access-Control-Max-Age')).toBe('3600');

      await app.stop();
    });
  });

  describe('actual requests', () => {
    it('should add CORS headers to responses with wildcard origin', async () => {
      const httpPlugin = http({ port: 0 });
      httpPlugin.use(CorsMiddleware());
      httpPlugin.get('/data', () => HttpResponse.json({ value: 42 }));

      const app = application().use(httpPlugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const res = await fetch(`${baseUrl}/data`, {
        headers: { Origin: 'https://example.com' },
      });
      expect(res.status).toBe(200);
      expect(res.headers.get('Access-Control-Allow-Origin')).toBe('*');

      await app.stop();
    });

    it('should restrict origin to allowed list', async () => {
      const httpPlugin = http({ port: 0 });
      httpPlugin.use(CorsMiddleware({ origin: ['https://allowed.com'] }));
      httpPlugin.get('/data', () => HttpResponse.json({ value: 42 }));

      const app = application().use(httpPlugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;

      const allowed = await fetch(`${baseUrl}/data`, {
        headers: { Origin: 'https://allowed.com' },
      });
      expect(allowed.headers.get('Access-Control-Allow-Origin')).toBe('https://allowed.com');
      expect(allowed.headers.get('Vary')).toBe('Origin');

      const denied = await fetch(`${baseUrl}/data`, {
        headers: { Origin: 'https://evil.com' },
      });
      expect(denied.headers.get('Access-Control-Allow-Origin')).toBeNull();

      await app.stop();
    });

    it('should support origin as a function', async () => {
      const httpPlugin = http({ port: 0 });
      httpPlugin.use(CorsMiddleware({ origin: (o) => o.endsWith('.example.com') }));
      httpPlugin.get('/data', () => HttpResponse.json({ value: 42 }));

      const app = application().use(httpPlugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const res = await fetch(`${baseUrl}/data`, {
        headers: { Origin: 'https://app.example.com' },
      });
      expect(res.headers.get('Access-Control-Allow-Origin')).toBe('https://app.example.com');

      await app.stop();
    });

    it('should set credentials header when enabled', async () => {
      const httpPlugin = http({ port: 0 });
      httpPlugin.use(CorsMiddleware({ origin: 'https://example.com', credentials: true }));
      httpPlugin.get('/data', () => HttpResponse.json({ value: 42 }));

      const app = application().use(httpPlugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const res = await fetch(`${baseUrl}/data`, {
        headers: { Origin: 'https://example.com' },
      });
      expect(res.headers.get('Access-Control-Allow-Credentials')).toBe('true');

      await app.stop();
    });

    it('should throw when credentials are enabled with wildcard origin', () => {
      expect(() => CorsMiddleware({ credentials: true })).toThrow(
        'CORS: origin "*" cannot be used with credentials:true',
      );
    });

    it('should set exposed headers', async () => {
      const httpPlugin = http({ port: 0 });
      httpPlugin.use(CorsMiddleware({ exposedHeaders: ['X-Custom', 'X-Request-Id'] }));
      httpPlugin.get('/data', () => HttpResponse.json({ value: 42 }));

      const app = application().use(httpPlugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const res = await fetch(`${baseUrl}/data`, {
        headers: { Origin: 'https://example.com' },
      });
      expect(res.headers.get('Access-Control-Expose-Headers')).toBe('X-Custom, X-Request-Id');

      await app.stop();
    });
  });
});
