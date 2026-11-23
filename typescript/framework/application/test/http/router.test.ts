import { describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { Router } from '../../src/http/router';

describe('Router', () => {
  it('should match simple routes', () => {
    const router = new Router<string>();
    router.add('/hello', 'hello-handler');
    const matches = router.find('/hello');
    expect(matches.length).toBe(1);
    expect(matches[0].handler).toBe('hello-handler');
  });

  it('should match parameter routes', () => {
    const router = new Router<string>();
    router.add('/users/[id]', 'user-handler');
    const matches = router.find('/users/123');
    expect(matches.length).toBe(1);
    expect(matches[0].handler).toBe('user-handler');
    expect(matches[0].params).toEqual({ id: '123' });
  });

  it('should distinguish between parent route and nested parameter route (The Fix)', () => {
    const router = new Router<string>();

    // Mimic the putnami.dev scenario
    // /docs/[package] -> Package Page
    // /docs/[package]/[topic] -> Topic Page

    router.add('/docs/[package]', 'package-handler');
    router.add('/docs/[package]/[topic]', 'topic-handler');

    // Case 1: /docs/core -> Should match ONLY package-handler
    const packageMatches = router.find('/docs/core');
    expect(packageMatches.length).toBe(1);
    expect(packageMatches[0].handler).toBe('package-handler');
    expect(packageMatches[0].params).toEqual({ package: 'core' });

    // Case 2: /docs/core/config -> Should match topic-handler
    // package-handler matches too (less specific)? standard router usually matches longest.
    // In this implementation, let's see what it returns.
    // Actually, usually specific handler takes precedence.
    const topicMatches = router.find('/docs/core/config');
    // It might return multiple matches sorted by score.
    expect(topicMatches.length).toBeGreaterThan(0);
    expect(topicMatches[0].handler).toBe('topic-handler');
    expect(topicMatches[0].params).toEqual({ package: 'core', topic: 'config' });
  });

  specTest(
    'should not match parameter route if parameter is missing',
    {
      feature: 'typescript/http-services',
      requirement: 'routing',
      check: 'a-parameter-matches-exactly-one-segment',
    },
    () => {
      const router = new Router<string>();
      router.add('/[a]/[b]', 'handler');

      // correctly matching
      const matchFull = router.find('/1/2');
      expect(matchFull.length).toBe(1);

      // missing last param
      const matchPartial = router.find('/1');
      expect(matchPartial.length).toBe(0);
    },
  );

  specTest(
    'should match parameter routes with suffix patterns like .json',
    {
      feature: 'typescript/http-services',
      requirement: 'routing',
      check: 'a-parameter-may-precede-a-literal-suffix',
    },
    () => {
      const router = new Router<string>();
      router.add('/docs/[package].json', 'json-handler');
      router.add('/docs/[package]', 'package-handler');

      // Match the .json suffix route
      const jsonMatches = router.find('/docs/core.json');
      expect(jsonMatches.length).toBeGreaterThan(0);
      expect(jsonMatches[0].handler).toBe('json-handler');
      expect(jsonMatches[0].params).toEqual({ package: 'core' });

      // Match the plain parameter route
      const plainMatches = router.find('/docs/core');
      expect(plainMatches.length).toBeGreaterThan(0);
      expect(plainMatches[0].handler).toBe('package-handler');
      expect(plainMatches[0].params).toEqual({ package: 'core' });
    },
  );
});

describe('Router static fast path', () => {
  it('matches an exact static path and reports a miss', () => {
    const router = new Router<string>();
    router.add('/api/health', 'health');
    router.add('/api/version', 'version');

    expect(router.find('/api/health').map((m) => m.handler)).toEqual(['health']);
    expect(router.find('/api/version').map((m) => m.handler)).toEqual(['version']);
    expect(router.find('/api/missing')).toEqual([]);
    // Trailing slash on the request normalises away and still hits the index.
    expect(router.find('/api/health/').map((m) => m.handler)).toEqual(['health']);
  });

  it('matches the root path through the fast path', () => {
    const router = new Router<string>();
    router.add('/', 'root');
    expect(router.find('/').map((m) => m.handler)).toEqual(['root']);
  });

  specTest(
    'applies content negotiation identically to the trie',
    {
      feature: 'typescript/http-services',
      requirement: 'routing',
      check: 'a-route-with-accept-types-matches-only-a-request-that-accepts-them',
    },
    () => {
      const router = new Router<string>();
      router.add('/data', 'json', { accept: ['application/json'] });

      expect(router.find('/data', ['application/json']).length).toBe(1);
      // No match and the request does not accept anything → filtered out.
      expect(router.find('/data', ['application/xml']).length).toBe(0);
      // Wildcard / empty Accept falls back to a 0.5 score and is kept.
      expect(router.find('/data', ['*/*']).length).toBe(1);
      expect(router.find('/data', []).length).toBe(1);
      expect(router.find('/data', ['*/*'])[0].score).toBeLessThan(router.find('/data', ['application/json'])[0].score);
    },
  );

  specTest(
    'matches a route that declares */* whatever the request accepts',
    {
      feature: 'typescript/http-services',
      requirement: 'wildcard-accept',
      check: 'a-wildcard-route-matches-any-accept-behind-a-named-type',
    },
    () => {
      const router = new Router<string>();
      router.add('/page', 'page', { accept: ['text/html', '*/*'] });
      router.add('/page', 'markdown', { accept: ['text/markdown'] });

      // A type the page does not name still reaches it through its */*.
      expect(router.find('/page', ['image/png']).map((m) => m.handler)).toEqual(['page']);
      // A route that names the requested type runs first.
      expect(router.find('/page', ['text/markdown']).map((m) => m.handler)).toEqual(['markdown', 'page']);

      // The trie path (a parameter route) scores the same way.
      const dynamic = new Router<string>();
      dynamic.add('/docs/[slug]', 'page', { accept: ['text/html', '*/*'] });
      expect(dynamic.find('/docs/intro', ['text/markdown']).map((m) => m.handler)).toEqual(['page']);

      // A less specific route that names the type still runs before a static
      // route that matches only through its */*.
      const mixed = new Router<string>();
      mixed.add('/feed', 'api', { accept: ['application/json', '*/*'] });
      mixed.add('/[name]', 'rss', { accept: ['application/rss+xml'] });
      expect(mixed.find('/feed', ['application/rss+xml']).map((m) => m.handler)).toEqual(['rss', 'api']);
      expect(mixed.find('/feed', ['application/json'])[0].handler).toBe('api');
    },
  );

  it('treats a route without an accept option as unconstrained', () => {
    const router = new Router<string>();
    router.add('/page', 'page');

    expect(router.find('/page', ['text/html']).map((match) => match.handler)).toEqual(['page']);
  });

  it('orders multiple content-type variants on the same static path by score', () => {
    const router = new Router<string>();
    router.add('/feed', 'specific', { accept: ['application/json'] });
    router.add('/feed', 'wild', { accept: ['*/*'] });

    // A `*/*` request keeps both: the `*/*` handler scores 1, the specific one
    // gets the 0.5 wildcard fallback, so the wildcard handler must sort first.
    const matches = router.find('/feed', ['*/*']);
    expect(matches.length).toBe(2);
    expect(matches[0].handler).toBe('wild');
    expect(matches[0].score).toBeGreaterThan(matches[1].score);
  });

  it('leaves a trailing-slash route unreachable (trie parity)', () => {
    const router = new Router<string>();
    router.add('/legacy/', 'legacy');
    // A trailing-slash route is unreachable through the trie; the fast path must
    // not resurrect it.
    expect(router.find('/legacy')).toEqual([]);
    expect(router.find('/legacy/')).toEqual([]);
  });

  specTest(
    'keeps cross-route fallthrough when a static and a dynamic route overlap',
    {
      feature: 'typescript/http-services',
      requirement: 'routing',
      check: 'the-most-specific-match-runs-first-and-the-rest-stay-for-fallthrough',
    },
    () => {
      const router = new Router<string>();
      router.add('/users/me', 'me');
      router.add('/users/[id]', 'by-id');

      // Static + dynamic match the same concrete path; the static route scores
      // higher but the dynamic candidate must remain available for fallthrough.
      const both = router.find('/users/me');
      expect(both[0].handler).toBe('me');
      expect(both.map((m) => m.handler)).toContain('by-id');

      // A different path matches only the dynamic route.
      const dynamic = router.find('/users/42');
      expect(dynamic.map((m) => m.handler)).toEqual(['by-id']);
      expect(dynamic[0].params).toEqual({ id: '42' });
    },
  );

  it('preserves fast-path results across a merge of static routers', () => {
    const a = new Router<string>();
    a.add('/a', 'a-handler');
    const b = new Router<string>();
    b.add('/b', 'b-handler');

    const merged = a.merge(b);
    expect(merged.find('/a').map((m) => m.handler)).toEqual(['a-handler']);
    expect(merged.find('/b').map((m) => m.handler)).toEqual(['b-handler']);
    expect(merged.find('/c')).toEqual([]);
  });

  it('falls back to the trie when a merge introduces a dynamic route', () => {
    const staticRouter = new Router<string>();
    staticRouter.add('/users/me', 'me');
    const dynamicRouter = new Router<string>();
    dynamicRouter.add('/users/[id]', 'by-id');

    const merged = staticRouter.merge(dynamicRouter);
    // Static path still resolves, and the dynamic overlap is still present.
    const both = merged.find('/users/me');
    expect(both[0].handler).toBe('me');
    expect(both.map((m) => m.handler)).toContain('by-id');
    expect(merged.find('/users/42').map((m) => m.handler)).toEqual(['by-id']);
  });
});

describe('Router static paths beside dynamic routes', () => {
  function walked(routes: [string, string][]): Router<string> {
    const router = new Router<string>();
    for (const [path, handler] of routes) router.add(path, handler);
    return router;
  }

  it('answers a static path no dynamic route matches exactly as the trie walk does', () => {
    const routes: [string, string][] = [
      ['/health', 'health'],
      ['/tasks/[id]', 'task'],
      ['/tasks/new', 'new-task'],
      ['/files/*', 'files'],
    ];
    const router = walked(routes);
    for (const path of ['/health', '/health/', '/tasks/new', '/tasks/42', '/files/a/b', '/missing']) {
      for (const accept of [[], ['*/*'], ['application/json']]) {
        const first = router.find(path, accept);
        // The second lookup of a path takes the cached fast-path decision.
        const second = router.find(path, accept);
        expect(second).toEqual(first);
      }
    }
    expect(router.find('/health').map((m) => m.handler)).toEqual(['health']);
    // A static route a dynamic route also matches keeps the fallthrough to it.
    expect(router.find('/tasks/new').map((m) => m.handler)).toEqual(['new-task', 'task']);
  });

  it('forgets a path answered alone once a route that also matches it is added', () => {
    const router = new Router<string>();
    router.add('/health', 'health');
    router.add('/tasks/[id]', 'task');
    expect(router.find('/health').map((m) => m.handler)).toEqual(['health']);

    router.add('/[page]', 'page');
    expect(router.find('/health').map((m) => m.handler)).toEqual(['health', 'page']);
  });
});

describe('Router static paths that share a trie node with another route', () => {
  const handlers = (router: Router<string>, path: string, accept?: string[]) =>
    router.find(path, accept).map((m) => m.handler);

  it('keeps a catch-all that also matches the root, whichever is added first', () => {
    for (const order of [
      ['/*', '/'],
      ['/', '/*'],
    ]) {
      const router = new Router<string>();
      for (const path of order) router.add(path, path === '/' ? 'root' : 'catch-all');
      const first = handlers(router, '/');
      expect(first).toContain('root');
      expect(first).toContain('catch-all');
      // The second lookup takes the cached decision and answers the same.
      expect(handlers(router, '/')).toEqual(first);
    }
  });

  it('keeps the catch-all of a merged router', () => {
    const merged = new Router<string>().add('/', 'root').merge(new Router<string>().add('/*', 'catch-all'));
    expect(handlers(merged, '/')).toContain('catch-all');
  });

  it('falls through to the catch-all when the static route does not accept the request', () => {
    const router = new Router<string>();
    router.add('/*', 'catch-all');
    router.add('/', 'page', { accept: ['text/html'] });
    expect(handlers(router, '/', ['text/html'])[0]).toBe('page');
    expect(handlers(router, '/', ['text/html'])).toContain('catch-all');
    expect(handlers(router, '/', ['application/json'])).toContain('catch-all');
  });

  it('keeps a group route that ends on the same path', () => {
    const router = new Router<string>();
    router.add('/(marketing)/about', 'group');
    router.add('/about', 'about');
    expect(handlers(router, '/about').sort()).toEqual(['about', 'group']);
  });
});

describe('Router root route beside a first-segment parameter', () => {
  const handlers = (router: Router<string>, path: string) => router.find(path).map((m) => m.handler);

  for (const order of [
    ['/', '/[slug]'],
    ['/[slug]', '/'],
  ]) {
    it(`matches the root only at / and the parameter elsewhere (added ${order.join(', ')})`, () => {
      const router = new Router<string>();
      for (const path of order) router.add(path, path === '/' ? 'root' : 'slug');
      expect(router.find('/')).toMatchObject([{ handler: 'root', route: '/' }]);
      expect(router.find('')).toMatchObject([{ handler: 'root', route: '/' }]);
      expect(router.find('/zz')).toMatchObject([{ handler: 'slug', route: '/[slug]', params: { slug: 'zz' } }]);
      expect(router.find('zz')).toMatchObject([{ handler: 'slug', route: '/[slug]', params: { slug: 'zz' } }]);
      // The cached exclusivity decision answers the same on a second lookup.
      expect(handlers(router, '/')).toEqual(['root']);
    });
  }

  it('keeps both routes apart in a merged router', () => {
    const merged = new Router<string>().add('/', 'root').merge(new Router<string>().add('/[slug]', 'slug'));
    expect(handlers(merged, '/')).toEqual(['root']);
    expect(merged.find('/zz')).toMatchObject([{ handler: 'slug', route: '/[slug]', params: { slug: 'zz' } }]);
  });

  it('keeps a nested parameter route apart from a static route on its parent', () => {
    const router = new Router<string>();
    router.add('/docs/[page]', 'page');
    router.add('/docs', 'docs');
    expect(handlers(router, '/docs')).toEqual(['docs']);
    expect(router.find('/docs/intro')).toMatchObject([{ handler: 'page', params: { page: 'intro' } }]);
  });
});
