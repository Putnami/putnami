import { describe, expect, it } from 'bun:test';
import { http } from '../../src/http/http.plugin';
import { HttpResponse } from '../../src/http/http-response';
import { CompressionMiddleware } from '../../src/http/compression.middleware';
import { CorsMiddleware } from '../../src/http/cors.middleware';
import { application } from '../../src/application';

describe('CompressionMiddleware', () => {
  it('should compress JSON responses above threshold', async () => {
    const largeData = { items: Array.from({ length: 100 }, (_, i) => ({ id: i, name: `item-${i}` })) };
    const httpPlugin = http({ port: 0 });
    httpPlugin.use(CompressionMiddleware({ threshold: 10 }));
    httpPlugin.get('/data', () => HttpResponse.json(largeData));

    const app = application().use(httpPlugin);
    await app.start();

    const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
    const res = await fetch(`${baseUrl}/data`, {
      headers: { 'Accept-Encoding': 'gzip, deflate' },
    });
    expect(res.status).toBe(200);
    expect(res.headers.get('Content-Encoding')).toBe('gzip');
    expect(res.headers.get('Vary')).toBe('Accept-Encoding');

    await app.stop();
  });

  it('should not compress responses below threshold', async () => {
    const httpPlugin = http({ port: 0 });
    httpPlugin.use(CompressionMiddleware({ threshold: 100_000 }));
    httpPlugin.get('/small', () => HttpResponse.json({ ok: true }));

    const app = application().use(httpPlugin);
    await app.start();

    const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
    const res = await fetch(`${baseUrl}/small`, {
      headers: { 'Accept-Encoding': 'gzip' },
    });
    expect(res.status).toBe(200);
    expect(res.headers.get('Content-Encoding')).toBeNull();

    await app.stop();
  });

  it('should not compress when client does not accept encoding', async () => {
    const largeData = { items: Array.from({ length: 100 }, (_, i) => ({ id: i })) };
    const httpPlugin = http({ port: 0 });
    httpPlugin.use(CompressionMiddleware({ threshold: 10 }));
    httpPlugin.get('/data', () => HttpResponse.json(largeData));

    const app = application().use(httpPlugin);
    await app.start();

    const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
    const res = await fetch(`${baseUrl}/data`, {
      headers: { 'Accept-Encoding': 'identity' },
    });
    expect(res.status).toBe(200);
    expect(res.headers.get('Content-Encoding')).toBeNull();

    await app.stop();
  });

  it('should use deflate when gzip is not accepted', async () => {
    const largeData = { items: Array.from({ length: 100 }, (_, i) => ({ id: i, name: `item-${i}` })) };
    const httpPlugin = http({ port: 0 });
    httpPlugin.use(CompressionMiddleware({ threshold: 10 }));
    httpPlugin.get('/data', () => HttpResponse.json(largeData));

    const app = application().use(httpPlugin);
    await app.start();

    const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
    const res = await fetch(`${baseUrl}/data`, {
      headers: { 'Accept-Encoding': 'deflate' },
    });
    expect(res.status).toBe(200);
    expect(res.headers.get('Content-Encoding')).toBe('deflate');

    await app.stop();
  });

  it('should preserve headers from downstream middleware', async () => {
    const httpPlugin = http({ port: 0 });
    httpPlugin.use(CompressionMiddleware({ threshold: 10 }));
    httpPlugin.use(CorsMiddleware({ origin: 'https://example.com', credentials: true }));
    httpPlugin.get('/data', () => HttpResponse.json({ ok: true }));

    const app = application().use(httpPlugin);
    await app.start();

    const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
    const res = await fetch(`${baseUrl}/data`, {
      headers: { 'Accept-Encoding': 'gzip', Origin: 'https://example.com' },
    });
    expect(res.status).toBe(200);
    expect(res.headers.get('Content-Encoding')).toBe('gzip');
    expect(res.headers.get('Access-Control-Allow-Origin')).toBe('https://example.com');
    expect(res.headers.get('Access-Control-Allow-Credentials')).toBe('true');

    await app.stop();
  });
});
