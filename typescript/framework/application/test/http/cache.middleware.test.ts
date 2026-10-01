import { describe, expect, it } from 'bun:test';
import { CacheMiddleware, buildCacheControl, computeETag } from '../../src/http/cache.middleware';
import { http } from '../../src/http/http.plugin';
import { HttpResponse } from '../../src/http/http-response';
import { application } from '../../src/application';

describe('buildCacheControl', () => {
  it('returns raw cacheControl string when provided', () => {
    expect(buildCacheControl({ cacheControl: 'no-store' })).toBe('no-store');
  });

  it('builds public max-age directive', () => {
    expect(buildCacheControl({ maxAge: 3600 })).toBe('public, max-age=3600');
  });

  it('builds private max-age directive', () => {
    expect(buildCacheControl({ privateMaxAge: 600 })).toBe('private, max-age=600');
  });

  it('appends s-maxage', () => {
    expect(buildCacheControl({ maxAge: 60, sMaxAge: 3600 })).toBe('public, max-age=60, s-maxage=3600');
  });

  it('appends stale-while-revalidate', () => {
    expect(buildCacheControl({ maxAge: 60, staleWhileRevalidate: 86_400 })).toBe(
      'public, max-age=60, stale-while-revalidate=86400',
    );
  });

  it('combines all shorthands', () => {
    expect(buildCacheControl({ maxAge: 60, sMaxAge: 300, staleWhileRevalidate: 3600 })).toBe(
      'public, max-age=60, s-maxage=300, stale-while-revalidate=3600',
    );
  });

  it('prefers cacheControl string over shorthands', () => {
    expect(buildCacheControl({ cacheControl: 'private, no-cache', maxAge: 3600 })).toBe('private, no-cache');
  });

  it('returns empty string when no options provided', () => {
    expect(buildCacheControl({})).toBe('');
  });

  it('prefers privateMaxAge over maxAge when both provided', () => {
    expect(buildCacheControl({ privateMaxAge: 300, maxAge: 600 })).toBe('private, max-age=300');
  });
});

describe('computeETag', () => {
  it('returns a quoted string', () => {
    const etag = computeETag('hello');
    expect(etag.startsWith('"')).toBe(true);
    expect(etag.endsWith('"')).toBe(true);
  });

  it('produces deterministic output', () => {
    expect(computeETag('test-data')).toBe(computeETag('test-data'));
  });

  it('produces different output for different inputs', () => {
    expect(computeETag('data-a')).not.toBe(computeETag('data-b'));
  });
});

describe('CacheMiddleware', () => {
  it('sets Cache-Control header on response', async () => {
    const httpPlugin = http({ port: 0 });
    httpPlugin.use(CacheMiddleware({ maxAge: 3600 }));
    httpPlugin.get('/test', () => 'ok');

    const app = application().use(httpPlugin);
    await app.start();

    const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
    const res = await fetch(`${baseUrl}/test`);
    expect(res.headers.get('Cache-Control')).toBe('public, max-age=3600');

    await app.stop();
  });

  it('sets private Cache-Control header', async () => {
    const httpPlugin = http({ port: 0 });
    httpPlugin.use(CacheMiddleware({ privateMaxAge: 600 }));
    httpPlugin.get('/test', () => 'ok');

    const app = application().use(httpPlugin);
    await app.start();

    const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
    const res = await fetch(`${baseUrl}/test`);
    expect(res.headers.get('Cache-Control')).toBe('private, max-age=600');

    await app.stop();
  });

  it('returns 304 when ETag matches If-None-Match', async () => {
    const httpPlugin = http({ port: 0 });
    httpPlugin.use(CacheMiddleware({ etag: true }));
    httpPlugin.get('/test', () => {
      const body = JSON.stringify({ data: 'test' });
      const etag = computeETag(body);
      return new HttpResponse(body, { headers: { ETag: etag, 'Content-Type': 'application/json' } });
    });

    const app = application().use(httpPlugin);
    await app.start();

    const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;

    // First request to get the ETag
    const res1 = await fetch(`${baseUrl}/test`);
    const etag = res1.headers.get('ETag');
    expect(etag).toBeTruthy();

    // Second request with If-None-Match
    const res2 = await fetch(`${baseUrl}/test`, {
      headers: { 'If-None-Match': etag! },
    });
    expect(res2.status).toBe(304);
    expect(res2.headers.get('ETag')).toBe(etag);

    await app.stop();
  });

  it('cancels a streamed response replaced by 304', async () => {
    let canceled = 0;
    const etag = '"stream"';
    const context = {
      headers: new Headers({ 'If-None-Match': etag }),
    } as Parameters<ReturnType<typeof CacheMiddleware>>[0];
    const response = await CacheMiddleware({ etag: true })(
      context,
      async () =>
        new HttpResponse(
          new ReadableStream<Uint8Array>({
            cancel() {
              canceled++;
            },
          }),
          { headers: { ETag: etag } },
        ),
    );
    expect(response?.status).toBe(304);
    expect(canceled).toBe(1);
  });

  it('does not set Cache-Control when no options match', async () => {
    const httpPlugin = http({ port: 0 });
    httpPlugin.use(CacheMiddleware({}));
    httpPlugin.get('/test', () => 'ok');

    const app = application().use(httpPlugin);
    await app.start();

    const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
    const res = await fetch(`${baseUrl}/test`);
    expect(res.headers.get('Cache-Control')).toBeNull();

    await app.stop();
  });
});
