import { describe, expect, it } from 'bun:test';
import type { HttpRequestContext } from '../../../src/http/http-context.type';
import { RouteController } from '../../../src/http/route.controller';
import { Router } from '../../../src/http/router';

describe('putnami-route', () => {
  it('errors', () => {
    const router = new Router<string>();
    expect(() => router.add(undefined as any, 'home')).toThrow();
  });
  it('simple path /a/b/c', () => {
    const router = new Router<string>();
    router.add('/', 'home').add('/a', 'a');
    expect(router.find('/')).toMatchObject([{ handler: 'home', route: '/', score: 5 }]);
    expect(router.find('/a')).toMatchObject([{ handler: 'a', route: '/a', score: 5 }]);
    expect(router.find('/no-found')).toEqual([]);
    router.add('/b/a', '/b/a').add('/b/b', '/b/b').add('/b', 'c');
    expect(router.find('/b')).toMatchObject([{ handler: 'c', route: '/b', score: 5 }]);
    expect(router.find('/b/a')).toMatchObject([{ handler: '/b/a', route: '/b/a', score: 10 }]);
    expect(router.find('/b/b')).toMatchObject([{ handler: '/b/b', route: '/b/b', score: 10 }]);
  });
  it('with params /a/[p1]/[p2]/b', () => {
    const router = new Router<string>();
    router.add('/a/[p1]/[p2]/b', 'c');
    expect(router.find('/a/1/2/b')).toMatchObject([
      {
        handler: 'c',
        params: { p1: '1', p2: '2' },
        route: '/a/[p1]/[p2]/b',
      },
    ]);
    expect(router.find('/a/1/2/c')).toEqual([]);
    router.add('/a/[p1]', '/a/[p1]');
    expect(router.find('/a/3')).toMatchObject([{ handler: '/a/[p1]', params: { p1: '3' } }]);
    expect(router.find('/a/3/1')).toEqual([]);
    router.add('/a/b', '/a/b');
    expect(router.find('/a/b')).toMatchObject([
      { handler: '/a/b', route: '/a/b', score: 10 },
      { handler: '/a/[p1]', route: '/a/[p1]', params: { p1: 'b' }, score: 0.3 },
    ]);
  });
  it('normalizes trailing slashes', () => {
    const router = new Router<string>();
    router.add('/docs', 'docs');
    router.add('/docs/[package]', 'package');

    expect(router.find('/docs/')).toMatchObject([{ handler: 'docs', route: '/docs' }]);
    expect(router.find('/docs/core/')).toMatchObject([
      { handler: 'package', route: '/docs/[package]', params: { package: 'core' } },
    ]);
  });
  it('add whildcoard /*', () => {
    let router = new Router<string>();
    router.add('/*', 'wild');
    router.add('/a/*', 'surper-wild');
    expect(router.find('/b/c')).toMatchObject([{ handler: 'wild' }]);
    expect(router.find('/a/b/c')).toMatchObject([{ handler: 'surper-wild' }, { handler: 'wild' }]);
    router = new Router<string>();
    router.add('/*', '/*').add('/', 'home');
    expect(router.find('/b')).toMatchObject([{ handler: '/*', route: '/' }]);
  });
  it('groups /a/(group)/b', () => {
    const router = new Router<string>();
    router.add('/a/(group)/b', 'group');
    expect(router.find('/a/oups/b')).toEqual([]);
    expect(router.find('/a/b')).toMatchObject([{ handler: 'group' }]);
  });
  it('mixt', () => {
    const router = new Router<string>();
    router.add('/*', '/*').add('/a', '/a').add('/a/c', '/a/c').add('/a/[b]', '/a/[b]');
    expect(router.find('/a/oups/b')).toMatchObject([{ handler: '/*' }]);
    expect(router.find('/a')).toMatchObject([{ handler: '/a' }, { handler: '/*' }]);
    expect(router.find('/a/c')).toMatchObject([
      { handler: '/a/c', params: undefined, route: '/a/c' },
      { handler: '/a/[b]', params: { b: 'c' } },
      { handler: '/*' },
    ]);
    expect(router.find('/a/b')).toMatchObject([{ handler: '/a/[b]' }, { handler: '/*' }]);
  });
  it('score match with accept', () => {
    const router = new Router<string>();
    router
      .add('/a', '/a[html]', { accept: ['html'] })
      .add('/a', '/a[json]', { accept: ['json'] })
      .add('/b', '/b[json]', { accept: ['json'] })
      .add('/b', '/b[html]', { accept: ['html'] });

    expect(router.find('/a', ['html'])).toMatchObject([{ handler: '/a[html]', score: 10 }]);
    expect(router.find('/a', ['json'])).toMatchObject([{ handler: '/a[json]', score: 10 }]);
    expect(router.find('/a', ['*/*'])).toMatchObject([
      { handler: '/a[html]', score: 5 },
      { handler: '/a[json]', score: 5 },
    ]);
    expect(router.find('/a', [])).toMatchObject([
      { handler: '/a[html]', score: 5 },
      { handler: '/a[json]', score: 5 },
    ]);

    expect(router.find('/b', ['html'])).toMatchObject([{ handler: '/b[html]', score: 10 }]);
    expect(router.find('/b', ['json'])).toMatchObject([{ handler: '/b[json]', score: 10 }]);
    expect(router.find('/b', ['*/*'])).toMatchObject([
      { handler: '/b[json]', score: 5 },
      { handler: '/b[html]', score: 5 },
    ]);
    expect(router.find('/b', [])).toMatchObject([
      { handler: '/b[json]', score: 5 },
      { handler: '/b[html]', score: 5 },
    ]);
  });
  it('merge router', () => {
    const rA = new Router<string>();
    rA.add('/a', '/a');
    const rB = new Router<string>();
    rB.add('/b', '/b');

    const merged = rA.merge(rB);
    expect(rB.find('/a').length).toEqual(0);
    expect(rB.find('/b')).toMatchObject([{ handler: '/b' }]);
    expect(rA.find('/b').length).toEqual(0);
    expect(rA.find('/a')).toMatchObject([{ handler: '/a' }]);
    expect(merged.find('/a')).toMatchObject([{ handler: '/a' }]);
    expect(merged.find('/b')).toMatchObject([{ handler: '/b' }]);
    expect(merged.find('/c').length).toEqual(0);
  });
});

const buildContext = (path: string, method = 'MESSAGE'): HttpRequestContext =>
  ({
    method: method as HttpRequestContext['method'],
    headers: new Headers(),
    path: () => path,
  }) as unknown as HttpRequestContext;

describe('RouteController.findFirst', () => {
  it('returns the highest-scoring (most specific) match', () => {
    const controller = new RouteController();
    // Concrete route and a catch-all wildcard both match /a/b.
    controller.route('MESSAGE' as HttpRequestContext['method'], '/a/[id]', (() => undefined) as any);
    controller.route('MESSAGE' as HttpRequestContext['method'], '/*', (() => undefined) as any);

    const all = controller.find(buildContext('/a/b'));
    // find() is ordered best-first: concrete param route outscores the wildcard.
    expect(all.length).toBeGreaterThan(1);
    expect(all[0].route).toBe('/a/[id]');
    expect(all[all.length - 1].route).toBe('/*');

    const first = controller.findFirst(buildContext('/a/b'));
    // findFirst must return the best (index 0), not the worst (last) match.
    expect(first?.route).toBe('/a/[id]');
  });

  it('returns undefined when nothing matches', () => {
    const controller = new RouteController();
    controller.route('MESSAGE' as HttpRequestContext['method'], '/a', (() => undefined) as any);
    expect(controller.findFirst(buildContext('/nope'))).toBeUndefined();
  });
});
