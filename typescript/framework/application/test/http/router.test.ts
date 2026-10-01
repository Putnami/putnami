import { describe, expect, it } from 'bun:test';
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

  it('should not match parameter route if parameter is missing', () => {
    const router = new Router<string>();
    router.add('/[a]/[b]', 'handler');

    // correctly matching
    const matchFull = router.find('/1/2');
    expect(matchFull.length).toBe(1);

    // missing last param
    const matchPartial = router.find('/1');
    expect(matchPartial.length).toBe(0);
  });

  it('should match parameter routes with suffix patterns like .json', () => {
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
  });
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

  it('applies content negotiation identically to the trie', () => {
    const router = new Router<string>();
    router.add('/data', 'json', { accept: ['application/json'] });

    expect(router.find('/data', ['application/json']).length).toBe(1);
    // No match and the request does not accept anything → filtered out.
    expect(router.find('/data', ['application/xml']).length).toBe(0);
    // Wildcard / empty Accept falls back to a 0.5 score and is kept.
    expect(router.find('/data', ['*/*']).length).toBe(1);
    expect(router.find('/data', []).length).toBe(1);
    expect(router.find('/data', ['*/*'])[0].score).toBeLessThan(router.find('/data', ['application/json'])[0].score);
  });

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

  it('keeps cross-route fallthrough when a static and a dynamic route overlap', () => {
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
  });

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
