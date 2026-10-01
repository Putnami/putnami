import { describe, expect, it } from 'bun:test';
import type { ContainerContext, DetachedScope, ScopeContext } from '@putnami/runtime';
import { SCOPE_CONTAINER_KEY } from '@putnami/runtime/inject';
import { HttpAbortException } from '../../src/http/http.exception';
import { attachRequestScope, buildHttpContext, closeRequestScope } from '../../src/http/http-context.builder';
import type { HttpRequestContext } from '../../src/http/http-context.type';

/** Minimal ContainerContext stub that counts scope creation/disposal. */
function fakeContainerContext() {
  const scope = { get: () => undefined, list: () => [], has: () => false } as unknown as ScopeContext;
  const counters = { created: 0, closed: 0 };
  const containerContext = {
    createScopeSync(): DetachedScope {
      counters.created++;
      return {
        scope,
        close: async () => {
          counters.closed++;
        },
      };
    },
  } as unknown as ContainerContext;
  return { containerContext, counters, scope };
}

function readScope(ctx: HttpRequestContext): ScopeContext | undefined {
  return (ctx as unknown as Record<symbol, ScopeContext | undefined>)[SCOPE_CONTAINER_KEY];
}

describe('buildHttpContext', () => {
  it('extracts method from request', () => {
    const req = new Request('http://localhost/test', { method: 'POST' });
    const ctx = buildHttpContext<HttpRequestContext>({ req });
    expect(ctx.method).toBe('POST');
  });

  it('extracts url from request', () => {
    const req = new Request('http://localhost/api/users');
    const ctx = buildHttpContext<HttpRequestContext>({ req });
    expect(ctx.url).toBe('http://localhost/api/users');
  });

  it('provides headers from request', () => {
    const req = new Request('http://localhost/test', {
      headers: { 'X-Custom': 'value' },
    });
    const ctx = buildHttpContext<HttpRequestContext>({ req });
    expect(ctx.headers.get('X-Custom')).toBe('value');
  });

  it('parses query params from URL', () => {
    const req = new Request('http://localhost/test?foo=bar&baz=1');
    const ctx = buildHttpContext<HttpRequestContext>({ req });
    const params = ctx.queryParams();
    expect(params['foo']).toBe('bar');
    expect(params['baz']).toBe('1');
  });

  it('caches queryParams on subsequent calls', () => {
    const req = new Request('http://localhost/test?foo=bar');
    const ctx = buildHttpContext<HttpRequestContext>({ req });
    const params1 = ctx.queryParams();
    const params2 = ctx.queryParams();
    expect(params1).toBe(params2); // Same reference
  });

  it('provides path accessor', () => {
    const req = new Request('http://localhost/api/users');
    const ctx = buildHttpContext<HttpRequestContext>({ req });
    expect(ctx.path()).toBe('api/users');
  });

  it('provides host accessor', () => {
    const req = new Request('http://localhost:3000/test');
    const ctx = buildHttpContext<HttpRequestContext>({ req });
    expect(ctx.host()).toBe('localhost:3000');
  });

  it('provides domain accessor', () => {
    const req = new Request('http://localhost:3000/test');
    const ctx = buildHttpContext<HttpRequestContext>({ req });
    expect(ctx.domain()).toBe('http://localhost:3000');
  });

  it('provides secured accessor', () => {
    const req = new Request('https://example.com/test');
    const ctx = buildHttpContext<HttpRequestContext>({ req });
    expect(ctx.secured()).toBe(true);
  });

  it('provides query accessor', () => {
    const req = new Request('http://localhost/test?foo=bar');
    const ctx = buildHttpContext<HttpRequestContext>({ req });
    expect(ctx.query()).toBe('?foo=bar');
  });

  it('throw method throws HttpAbortException', () => {
    const req = new Request('http://localhost/test');
    const ctx = buildHttpContext<HttpRequestContext>({ req });
    expect(() => ctx.throw(404, 'Not Found')).toThrow(HttpAbortException);
  });

  it('captures Authorization header', () => {
    const req = new Request('http://localhost/test', {
      headers: { Authorization: 'Bearer token123' },
    });
    const ctx = buildHttpContext<HttpRequestContext>({ req });
    expect(ctx.__authorizationHeader).toBe('Bearer token123');
  });

  it('sets undefined __authorizationHeader when no auth header', () => {
    const req = new Request('http://localhost/test');
    const ctx = buildHttpContext<HttpRequestContext>({ req });
    expect(ctx.__authorizationHeader).toBeUndefined();
  });

  it('parses JSON body', async () => {
    const req = new Request('http://localhost/test', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name: 'test' }),
    });
    const ctx = buildHttpContext<HttpRequestContext>({ req });
    const body = await ctx.body<{ name: string }>();
    expect(body).toEqual({ name: 'test' });
  });

  it('parses JSON body when charset param has no leading space', async () => {
    // Regression: media-type parsing must not require '; ' before params.
    // `application/json;charset=utf-8` (no space) previously fell through and
    // the body was silently dropped, so validation ran against {}.
    const req = new Request('http://localhost/test', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json;charset=utf-8' },
      body: JSON.stringify({ name: 'test' }),
    });
    const ctx = buildHttpContext<HttpRequestContext>({ req });
    const body = await ctx.body<{ name: string }>();
    expect(body).toEqual({ name: 'test' });
  });

  it('parses JSON body when media type is uppercased with charset', async () => {
    const req = new Request('http://localhost/test', {
      method: 'POST',
      headers: { 'Content-Type': 'Application/JSON; charset=UTF-8' },
      body: JSON.stringify({ name: 'test' }),
    });
    const ctx = buildHttpContext<HttpRequestContext>({ req });
    const body = await ctx.body<{ name: string }>();
    expect(body).toEqual({ name: 'test' });
  });

  it('returns undefined for non-JSON body without content-type', async () => {
    const req = new Request('http://localhost/test', {
      method: 'POST',
      body: 'plain text',
    });
    const ctx = buildHttpContext<HttpRequestContext>({ req });
    const body = await ctx.body();
    expect(body).toBeUndefined();
  });

  it('rejects body exceeding maxBodySizeBytes via Content-Length', async () => {
    const req = new Request('http://localhost/test', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        'Content-Length': '1000000',
      },
      body: JSON.stringify({ data: 'x'.repeat(100) }),
    });
    const ctx = buildHttpContext<HttpRequestContext>({ req, maxBodySizeBytes: 1024 });
    await expect(ctx.body()).rejects.toThrow();
  });

  it('uses signal from context when provided', () => {
    const controller = new AbortController();
    const req = new Request('http://localhost/test');
    const ctx = buildHttpContext<HttpRequestContext>({ req, signal: controller.signal });
    expect(ctx.signal).toBe(controller.signal);
  });

  it('accessors keep working when extracted unbound', () => {
    // The logger middleware destructures these, and the WS/SSE/stream/gRPC layers
    // copy them into detached sub-contexts — i.e. they are called without the
    // original `this`. They must remain bound closures, not prototype methods.
    const req = new Request('https://example.com:3000/api/items?q=1');
    const ctx = buildHttpContext<HttpRequestContext>({ req });
    const { path, query, host, domain, secured } = ctx;
    expect(path()).toBe('api/items');
    expect(query()).toBe('?q=1');
    expect(host()).toBe('example.com:3000');
    expect(domain()).toBe('https://example.com:3000');
    expect(secured()).toBe(true);
  });
});

describe('buildHttpContext lazy DI scope', () => {
  it('does not create a scope when none is attached', () => {
    const ctx = buildHttpContext<HttpRequestContext>({ req: new Request('http://localhost/test') });
    // No DI context attached → reading the scope yields undefined (useContainer throws on this).
    expect(readScope(ctx)).toBeUndefined();
    // Closing is a no-op and must not throw.
    expect(closeRequestScope(ctx)).toBeUndefined();
  });

  it('does not materialise a scope for a request that never resolves', async () => {
    const { containerContext, counters } = fakeContainerContext();
    const ctx = buildHttpContext<HttpRequestContext>({ req: new Request('http://localhost/test') });
    attachRequestScope(ctx, containerContext);

    // Never reading the scope means it is never created.
    await closeRequestScope(ctx);
    expect(counters.created).toBe(0);
    expect(counters.closed).toBe(0);
  });

  it('materialises the scope on first access and reuses it', async () => {
    const { containerContext, counters, scope } = fakeContainerContext();
    const ctx = buildHttpContext<HttpRequestContext>({ req: new Request('http://localhost/test') });
    attachRequestScope(ctx, containerContext);

    expect(readScope(ctx)).toBe(scope);
    expect(counters.created).toBe(1);
    // Subsequent reads reuse the same scope without recreating it.
    expect(readScope(ctx)).toBe(scope);
    expect(counters.created).toBe(1);

    await closeRequestScope(ctx);
    expect(counters.closed).toBe(1);
  });
});
