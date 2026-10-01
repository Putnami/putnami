import { beforeEach, describe, expect, it, mock } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { cache, clearCache, HttpResponse } from '@putnami/application';
import {
  buildJsonResponseWithCache,
  evictPatterns,
  executeLoaderWithCache,
  normalizeRoute,
  serializeParams,
  wrapPageWithCache,
} from '../../src/ssr/route-cache.utils';

function createMockContext(overrides: Record<string, unknown> = {}) {
  return {
    headers: new Headers(),
    params: {},
    queryParams: () => ({}),
    body: async () => undefined,
    statusCode: 0,
    ...overrides,
  } as never;
}

describe('route-cache.utils', () => {
  beforeEach(async () => {
    await clearCache();
  });

  describe('normalizeRoute', () => {
    it('adds leading slash if missing', () => {
      expect(normalizeRoute('users')).toBe('/users');
    });

    it('keeps existing leading slash', () => {
      expect(normalizeRoute('/users')).toBe('/users');
    });

    it('handles empty string', () => {
      expect(normalizeRoute('')).toBe('/');
    });
  });

  describe('serializeParams', () => {
    it('returns empty string for undefined', () => {
      expect(serializeParams(undefined)).toBe('');
    });

    it('returns empty string for empty object', () => {
      expect(serializeParams({})).toBe('');
    });

    it('serializes single param', () => {
      expect(serializeParams({ id: '123' })).toBe('id=123');
    });

    it('sorts params alphabetically', () => {
      expect(serializeParams({ z: '1', a: '2', m: '3' })).toBe('a=2&m=3&z=1');
    });

    specTest(
      'produces stable output regardless of insertion order',
      {
        feature: 'typescript/web-application-delivery',
        requirement: 'cache-isolation',
        check: 'a-loader-cache-key-serializes-its-params-in-a-stable-order',
      },
      () => {
        const a = serializeParams({ b: '2', a: '1' });
        const b = serializeParams({ a: '1', b: '2' });
        expect(a).toBe(b);
      },
    );
  });

  describe('buildJsonResponseWithCache', () => {
    it('adds ETag and Cache-Control headers to JSON responses', async () => {
      const response = buildJsonResponseWithCache({ ok: true }, createMockContext(), {
        etag: true,
        maxAge: 60,
        staleWhileRevalidate: 30,
      });

      const native = response.get();
      expect(native.status).toBe(200);
      expect(native.headers.get('etag')).toBeTruthy();
      expect(native.headers.get('cache-control')).toContain('max-age=60');
      expect(native.headers.get('cache-control')).toContain('stale-while-revalidate=30');
      expect(await native.json()).toEqual({ ok: true });
    });

    it('returns 304 when the ETag matches the request header', () => {
      const ctx = createMockContext({
        headers: new Headers({ 'If-None-Match': 'custom-etag' }),
      });

      const response = buildJsonResponseWithCache({ ok: true }, ctx, {
        etag: () => 'custom-etag',
      });

      expect(response.status).toBe(304);
      expect(response.get().headers.get('etag')).toBe('custom-etag');
    });
  });

  describe('executeLoaderWithCache', () => {
    specTest(
      'does not cache loaders when ttl is not set',
      {
        feature: 'typescript/web-application-delivery',
        requirement: 'cache-isolation',
        check: 'a-loader-is-not-cached-without-a-ttl',
      },
      async () => {
        const loader = mock(() => ({ value: Math.random() }));
        const ctx = createMockContext();

        await executeLoaderWithCache(loader, ctx, '/users');
        await executeLoaderWithCache(loader, ctx, '/users');

        expect(loader).toHaveBeenCalledTimes(2);
      },
    );

    it('caches loaders when ttl is configured', async () => {
      const loader = mock(() => ({ value: Date.now() }));
      const ctx = createMockContext({ params: { id: '42' } });

      const first = await executeLoaderWithCache(loader, ctx, '/users/[id]', { ttl: 60_000 });
      const second = await executeLoaderWithCache(loader, ctx, '/users/[id]', { ttl: 60_000 });

      expect(loader).toHaveBeenCalledTimes(1);
      expect(second).toEqual(first);
    });

    it('uses a custom cache key when provided', async () => {
      const loader = mock(() => ({ cached: true }));
      const ctx = createMockContext({ params: { id: 'abc' } });

      await executeLoaderWithCache(loader, ctx, '/users/[id]', {
        ttl: 60_000,
        key: (requestContext) => `user:${requestContext.params.id}`,
      });
      await executeLoaderWithCache(loader, ctx, '/users/[id]', {
        ttl: 60_000,
        key: (requestContext) => `user:${requestContext.params.id}`,
      });

      expect(loader).toHaveBeenCalledTimes(1);
    });
  });

  describe('evictPatterns', () => {
    specTest(
      'evicts both static and computed cache keys',
      {
        feature: 'typescript/web-application-delivery',
        requirement: 'cache-isolation',
        check: 'a-successful-action-evicts-both-static-and-computed-keys',
      },
      async () => {
        await cache('page')
          .ttl(60_000)
          .for('foo')
          .fetch(() => 'cached-foo');
        await cache('page')
          .ttl(60_000)
          .for('bar')
          .fetch(() => 'cached-bar');

        await evictPatterns(['page:foo', () => 'page:bar']);

        const foo = await cache('page')
          .ttl(60_000)
          .for('foo')
          .fetch(() => 'fresh-foo');
        const bar = await cache('page')
          .ttl(60_000)
          .for('bar')
          .fetch(() => 'fresh-bar');

        expect(foo).toBe('fresh-foo');
        expect(bar).toBe('fresh-bar');
      },
    );

    it('does nothing when there are no patterns', async () => {
      await expect(evictPatterns(undefined)).resolves.toBeUndefined();
      await expect(evictPatterns([])).resolves.toBeUndefined();
    });
  });

  describe('wrapPageWithCache', () => {
    it('returns the original renderer when ttl is not configured', () => {
      const renderer = mock(async () => new HttpResponse('ok'));
      expect(wrapPageWithCache(renderer, '/users', undefined)).toBe(renderer);
    });

    it('caches rendered HTML and preserves status codes', async () => {
      const renderer = mock(async () => new HttpResponse('<h1>Users</h1>', { status: 201 }));
      const wrapped = wrapPageWithCache(renderer, '/users', { ttl: 60_000 });
      const ctx = createMockContext({ params: { page: '1' } });

      const first = await wrapped(ctx);
      const second = await wrapped(ctx);

      expect(renderer).toHaveBeenCalledTimes(1);
      expect(first.status).toBe(201);
      expect(second.status).toBe(201);
      const firstNative = first.get();
      const secondNative = second.get();
      expect(await firstNative.text()).toBe('<h1>Users</h1>');
      expect(await secondNative.text()).toBe('<h1>Users</h1>');
      expect(firstNative.headers.get('content-type')).toBe('text/html;charset=utf-8');
    });

    it('uses a custom page cache key when provided', async () => {
      const renderer = mock(async () => new HttpResponse('<h1>User</h1>'));
      const wrapped = wrapPageWithCache(renderer, '/users/[id]', {
        ttl: 60_000,
        key: (ctx) => `profile:${ctx.params.id}`,
      });
      const ctx = createMockContext({ params: { id: '42' } });

      await wrapped(ctx);
      await wrapped(ctx);

      expect(renderer).toHaveBeenCalledTimes(1);
    });

    specTest(
      'keys the default cache by user identity to prevent cross-user leakage',
      {
        feature: 'typescript/web-application-delivery',
        requirement: 'cache-isolation',
        check: 'the-default-page-cache-key-includes-a-user-fingerprint',
      },
      async () => {
        let n = 0;
        const renderer = mock(async () => new HttpResponse(`<h1>render-${++n}</h1>`));
        const wrapped = wrapPageWithCache(renderer, '/dashboard', { ttl: 60_000 });

        const alice = createMockContext({ user: { sub: 'alice', roles: ['admin'] } });
        const bob = createMockContext({ user: { sub: 'bob', roles: ['user'] } });

        const aliceRes = await wrapped(alice);
        const bobRes = await wrapped(bob);

        // Different users must not share a cache entry: the renderer runs again
        // for Bob and he never receives Alice's cached HTML.
        expect(renderer).toHaveBeenCalledTimes(2);
        expect(await aliceRes.get().text()).toBe('<h1>render-1</h1>');
        expect(await bobRes.get().text()).toBe('<h1>render-2</h1>');

        // The same user reuses their own cache entry.
        const aliceAgain = await wrapped(alice);
        expect(renderer).toHaveBeenCalledTimes(2);
        expect(await aliceAgain.get().text()).toBe('<h1>render-1</h1>');
      },
    );

    specTest(
      'uses a stable user fingerprint for equivalent claims objects',
      {
        feature: 'typescript/web-application-delivery',
        requirement: 'cache-isolation',
        check: 'the-user-fingerprint-is-stable-for-equivalent-claims',
      },
      async () => {
        let n = 0;
        const renderer = mock(async () => new HttpResponse(`<h1>render-${++n}</h1>`));
        const wrapped = wrapPageWithCache(renderer, '/dashboard', { ttl: 60_000 });

        const first = createMockContext({
          user: { sub: 'alice', roles: ['admin'], profile: { b: 2, a: 1 } },
        });
        const sameClaimsDifferentOrder = createMockContext({
          user: { profile: { a: 1, b: 2 }, roles: ['admin'], sub: 'alice' },
        });

        const firstRes = await wrapped(first);
        const secondRes = await wrapped(sameClaimsDifferentOrder);

        expect(renderer).toHaveBeenCalledTimes(1);
        expect(await firstRes.get().text()).toBe('<h1>render-1</h1>');
        expect(await secondRes.get().text()).toBe('<h1>render-1</h1>');
      },
    );

    specTest(
      'keys the default cache by query parameters to avoid stale loader hydration data',
      {
        feature: 'typescript/web-application-delivery',
        requirement: 'cache-isolation',
        check: 'the-default-page-cache-key-includes-the-query-string',
      },
      async () => {
        let n = 0;
        const renderer = mock(async () => new HttpResponse(`<h1>render-${++n}</h1>`));
        const wrapped = wrapPageWithCache(renderer, '/search', { ttl: 60_000 });

        const alpha = createMockContext({ queryParams: () => ({ q: 'alpha' }) });
        const beta = createMockContext({ queryParams: () => ({ q: 'beta' }) });

        const alphaRes = await wrapped(alpha);
        const betaRes = await wrapped(beta);

        expect(renderer).toHaveBeenCalledTimes(2);
        expect(await alphaRes.get().text()).toBe('<h1>render-1</h1>');
        expect(await betaRes.get().text()).toBe('<h1>render-2</h1>');
      },
    );

    specTest(
      'does not cache redirect responses',
      {
        feature: 'typescript/web-application-delivery',
        requirement: 'cache-isolation',
        check: 'a-redirect-is-never-cached',
      },
      async () => {
        const renderer = mock(async () => HttpResponse.redirect('/login', 302));
        const wrapped = wrapPageWithCache(renderer, '/private', { ttl: 60_000 });
        const ctx = createMockContext();

        const first = await wrapped(ctx);
        const second = await wrapped(ctx);

        expect(renderer).toHaveBeenCalledTimes(2);
        expect(first.redirected).toBe(true);
        expect(second.redirected).toBe(true);
      },
    );
  });
});
