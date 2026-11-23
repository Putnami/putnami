import { describe, expect, it } from 'bun:test';
import { endpoint } from '../../src/api/route/endpoint';
import { application } from '../../src/application';
import { CsrfMiddleware } from '../../src/http/csrf.middleware';
import type { HttpRequestContext, HttpRequestContextInternal } from '../../src/http/http-context.type';
import { HttpMethodsMiddleware } from '../../src/http/http-methods.middleware';
import type { HttpMiddleware } from '../../src/http/http-middleware.type';
import { http } from '../../src/http/http.plugin';
import { HttpResponse } from '../../src/http/http-response';
import { OriginGuardMiddleware } from '../../src/http/origin-guard.middleware';
import { RouteController } from '../../src/http/route.controller';
import { actsOnRoutes, RouteChains, type RouteTraits, routeMayMatch } from '../../src/http/route-scope';
import { platform } from '../../src/platform/platform.plugin';
import { SecurityHeadersMiddleware } from '../../src/http/security-headers.middleware';

const route = (overrides: Partial<RouteTraits>): RouteTraits => ({
  method: 'GET',
  route: '/items',
  minimal: false,
  securityHeaders: true,
  csrfExempt: false,
  ...overrides,
});

/** The built-in middlewares of a secure server with CSRF on, in their server order, plus one user middleware. */
function builtIns() {
  const headers = SecurityHeadersMiddleware();
  const guard = OriginGuardMiddleware();
  const user: HttpMiddleware = (_ctx, next) => next();
  const csrf = CsrfMiddleware();
  const methods = HttpMethodsMiddleware(new RouteController(), { head: true, options: true, trace: false });
  return { headers, guard, user, csrf, methods, all: [headers, guard, user, csrf, methods] };
}

describe('route chains', () => {
  it('a GET api route with no CORS, CSRF or rate limit configured runs none of those middlewares', () => {
    const { headers, user, all } = builtIns();
    const chains = new RouteChains(all);
    // An api() route is CSRF exempt and its endpoint configures no CORS or rate limit.
    const definition = endpoint().handle(() => ({ ok: true }));
    expect(definition.middleware ?? []).toHaveLength(0);
    expect(chains.middlewaresFor(route({ csrfExempt: true }))).toEqual([headers, user]);
  });

  it('an endpoint that configures CORS and a rate limit carries both, and only then', () => {
    const definition = endpoint()
      .cors()
      .rateLimit({ max: 10 })
      .handle(() => ({ ok: true }));
    expect(definition.middleware).toHaveLength(2);
  });

  it('a POST route that is not exempt runs the origin guard and CSRF, not the HTTP methods middleware', () => {
    const { headers, guard, user, csrf, all } = builtIns();
    const chains = new RouteChains(all);
    expect(chains.middlewaresFor(route({ method: 'POST' }))).toEqual([headers, guard, user, csrf]);
  });

  it('a page GET route keeps CSRF, which issues its token on safe methods', () => {
    const { headers, user, csrf, all } = builtIns();
    expect(new RouteChains(all).middlewaresFor(route({}))).toEqual([headers, user, csrf]);
  });

  it('a minimal route skips the security headers and the origin guard', () => {
    const { user, csrf, all } = builtIns();
    expect(new RouteChains(all).middlewaresFor(route({ method: 'PUT', minimal: true }))).toEqual([user, csrf]);
  });

  it('a route that opts out of the security headers skips them only', () => {
    const { guard, user, all } = builtIns();
    const traits = route({ method: 'DELETE', securityHeaders: false, csrfExempt: true });
    expect(new RouteChains(all).middlewaresFor(traits)).toEqual([guard, user]);
  });

  it('an explicit HEAD or OPTIONS route keeps the HTTP methods middleware', () => {
    const { methods, all } = builtIns();
    const chains = new RouteChains(all);
    expect(chains.middlewaresFor(route({ method: 'HEAD', csrfExempt: true }))).toContain(methods);
    expect(chains.middlewaresFor(route({ method: 'OPTIONS', csrfExempt: true }))).toContain(methods);
  });

  it('runs the full chain for an unmatched request or several candidates, and caches a route chain', () => {
    const scoped = actsOnRoutes(
      (_ctx, next) => next(),
      (r) => r.csrfExempt,
    );
    const chains = new RouteChains([scoped]);
    const candidate = { route: '/a', handler: () => undefined, accept: new Set<string>(), statusCode: 200, score: 1 };
    const ctx = (matched: unknown[] | undefined) =>
      ({ method: 'GET', __matchedHandlers: matched }) as unknown as HttpRequestContextInternal as HttpRequestContext;

    expect(chains.select(ctx(undefined))).toBe(chains.full);
    expect(chains.select(ctx([]))).toBe(chains.full);
    expect(chains.select(ctx([candidate, { ...candidate, route: '/[x]' }]))).toBe(chains.full);
    const single = chains.select(ctx([candidate]));
    expect(single).not.toBe(chains.full);
    expect(chains.select(ctx([{ ...candidate }]))).toBe(single);
    const exempt = chains.select(ctx([{ ...candidate, csrfExempt: true }]));
    expect(exempt).not.toBe(single);
    expect(chains.select(ctx([{ ...candidate, csrfExempt: true }]))).toBe(exempt);
  });

  it('a matched route never calls a middleware that does not act on it; other requests still do', async () => {
    const calls: string[] = [];
    const postOnly = actsOnRoutes(
      async (ctx, next) => {
        calls.push(`postOnly ${ctx.method} ${ctx.path()}`);
        return next();
      },
      (r) => r.method === 'POST',
    );
    const everywhere: HttpMiddleware = async (ctx, next) => {
      calls.push(`everywhere ${ctx.method} ${ctx.path()}`);
      return next();
    };
    const plugin = http({ port: 0 }).use(postOnly).use(everywhere);
    plugin.get('/items', () => ({ ok: true }));
    plugin.post('/items', () => ({ created: true }));
    const app = application().use(plugin);
    await app.start();
    try {
      const base = `http://localhost:${plugin.getServer()?.port}`;
      expect((await fetch(`${base}/items`)).status).toBe(200);
      expect((await fetch(`${base}/items`, { method: 'POST' })).status).toBe(200);
      expect((await fetch(`${base}/missing`)).status).toBe(404);
      expect(calls).toEqual([
        'everywhere GET items',
        'postOnly POST items',
        'everywhere POST items',
        'postOnly GET missing',
        'everywhere GET missing',
      ]);
    } finally {
      await app.stop();
    }
  });

  it('keeps the secure defaults on routes whose chain is reduced', async () => {
    const plugin = http({ port: 0, csrf: true });
    plugin.get('/page', () => new HttpResponse('<p>hi</p>', { headers: { 'Content-Type': 'text/html' } }));
    plugin.get('/api/items', () => ({ ok: true }), { csrfExempt: true });
    plugin.post('/api/items', () => ({ ok: true }), { csrfExempt: true });
    const app = application().use(plugin);
    await app.start();
    try {
      const base = `http://localhost:${plugin.getServer()?.port}`;
      const page = await fetch(`${base}/page`);
      expect(page.headers.get('X-Frame-Options')).toBe('DENY');
      expect(page.headers.get('Set-Cookie')).toContain('_csrf=');
      await page.arrayBuffer();

      const items = await fetch(`${base}/api/items`);
      expect(items.headers.get('X-Content-Type-Options')).toBe('nosniff');
      expect(items.headers.get('Set-Cookie')).toBeNull();
      await items.arrayBuffer();

      const crossSite = await fetch(`${base}/api/items`, {
        method: 'POST',
        headers: { Origin: 'https://evil.com', 'Sec-Fetch-Site': 'cross-site' },
      });
      expect(crossSite.status).toBe(403);
      await crossSite.arrayBuffer();

      const head = await fetch(`${base}/api/items`, { method: 'HEAD' });
      expect(head.status).toBe(200);
      const options = await fetch(`${base}/api/items`, { method: 'OPTIONS' });
      expect(options.status).toBe(204);
      expect(options.headers.get('Allow')).toContain('POST');
      const notAllowed = await fetch(`${base}/api/items`, { method: 'PUT' });
      expect(notAllowed.status).toBe(405);
      await notAllowed.arrayBuffer();
    } finally {
      await app.stop();
    }
  });

  it('tells whether a route pattern may match a path', () => {
    expect(routeMayMatch('/health', '/livez')).toBe(false);
    expect(routeMayMatch('/livez', '/livez')).toBe(true);
    expect(routeMayMatch('/[slug]', '/livez')).toBe(true);
    expect(routeMayMatch('/users/[id]', '/livez')).toBe(false);
    expect(routeMayMatch('/[...rest]', '/livez')).toBe(true);
    expect(routeMayMatch('/docs/(lang)', '/livez')).toBe(true);
  });

  it('keeps the platform probes on their paths, and on a route that may shadow one', async () => {
    const plugin = http({ port: 0 });
    plugin.get('/items', () => ({ items: [] }));
    plugin.get('/[slug]', (ctx) => ({ slug: ctx.params?.['slug'] }));
    const app = application().use(plugin).use(platform());
    await app.start();
    try {
      const base = `http://localhost:${plugin.getServer()?.port}`;
      expect(await (await fetch(`${base}/items`)).json()).toEqual({ items: [] });
      const livez = await (await fetch(`${base}/livez`)).json();
      expect(livez).not.toEqual({ slug: 'livez' });
      expect(await (await fetch(`${base}/other`)).json()).toEqual({ slug: 'other' });
    } finally {
      await app.stop();
    }
  });
});
