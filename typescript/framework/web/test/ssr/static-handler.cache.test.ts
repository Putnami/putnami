import { afterAll, beforeAll, describe, expect, it } from 'bun:test';
import { application, HttpPlugin, type HttpRequestContext, type HttpResponse } from '@putnami/application';
import { resolve } from 'node:path';
import React from 'react';
import { Outlet, type RouteObject } from 'react-router';
import { buildHttpContext } from '../../../application/src/http/http-context.builder';
import { runInContext } from '../../../runtime/src/context/context.utils';
import RootHtml from '../../src/ssr/default.html';
import { pageRenderer } from '../../src/ssr/page.renderer';
import { ReactApplication } from '../../src/ssr/react-application';
import { createStaticHandlerCache, type StaticHandlerCache } from '../../src/ssr/static-handler.cache';
import { createMockReactConfig } from '../fixtures/route.fixtures';

/** A cache that counts the trees it builds a handler from. */
function countingCache(): { cache: StaticHandlerCache; builds: () => number } {
  const inner = createStaticHandlerCache();
  let builds = 0;
  return {
    cache: {
      handler: (source, key, tree, basename) =>
        inner.handler(
          source,
          key,
          () => {
            builds++;
            return tree();
          },
          basename,
        ),
      invalidate: () => inner.invalidate(),
    },
    builds: () => builds,
  };
}

describe('static handler cache', () => {
  const tree: RouteObject[] = [{ path: '/', element: React.createElement('div', null, 'home') }];

  it('returns the handler it built for the same tree and key', () => {
    const cache = createStaticHandlerCache();
    const first = cache.handler(tree, 'en', () => tree, undefined);
    expect(cache.handler(tree, 'en', () => tree, undefined)).toBe(first);
  });

  it('keeps one handler per key', () => {
    const cache = createStaticHandlerCache();
    const en = cache.handler(tree, 'en', () => tree, undefined);
    const fr = cache.handler(tree, 'fr', () => tree, '/fr');
    expect(fr).not.toBe(en);
    expect(cache.handler(tree, 'en', () => tree, undefined)).toBe(en);
    expect(cache.handler(tree, 'fr', () => tree, '/fr')).toBe(fr);
  });

  it('rebuilds once invalidated or for another tree', () => {
    const cache = createStaticHandlerCache();
    const first = cache.handler(tree, 'en', () => tree, undefined);
    cache.invalidate();
    const second = cache.handler(tree, 'en', () => tree, undefined);
    expect(second).not.toBe(first);
    const other: RouteObject[] = [{ path: '/' }];
    expect(cache.handler(other, 'en', () => other, undefined)).not.toBe(second);
  });

  it('builds the handler of a page renderer once across requests', async () => {
    const { cache, builds } = countingCache();
    const handler = pageRenderer(
      () => tree,
      () => RootHtml,
      () => 'hydrate.js',
      500,
      undefined,
      undefined,
      undefined,
      undefined,
      cache,
    );
    for (let i = 0; i < 3; i++) {
      const ctx = buildHttpContext<HttpRequestContext>({ req: new Request('http://localhost/') });
      Object.assign(ctx, { params: {}, statusCode: 0 });
      // biome-ignore lint/performance/noAwaitInLoops: requests run one after the other
      const response = (await runInContext(ctx, () => handler(ctx))) as HttpResponse;
      expect(await response.get().text()).toContain('<div>home</div>');
    }
    expect(builds()).toBe(1);
  });
});

describe('application route tree changes', () => {
  const packageRoot = resolve(import.meta.dir, '..', '..');
  let server: { stop: () => Promise<void> };
  let origin = '';
  let app: ReactApplication;

  beforeAll(async () => {
    process.env['PUTNAMI_WORKSPACE_ROOT'] = resolve(packageRoot, '..', '..', '..');
    process.env['PUTNAMI_PROJECT_ROOT'] = packageRoot;
    const root = new HttpPlugin({ port: 0, secure: false });
    app = new ReactApplication(createMockReactConfig({ isDevelopment: true }), new HttpPlugin());
    app.reactPage('/', { page: { default: () => React.createElement('p', null, 'home') } });
    root.merge(app.getHttpPlugin());
    const composed = application().use(root);
    await composed.start();
    server = composed;
    origin = `http://localhost:${root.getServer()?.port}`;
  });

  afterAll(async () => {
    await server.stop();
  });

  const get = async (path: string) => {
    const response = await fetch(`${origin}${path}`, { headers: { accept: 'text/html' } });
    return { status: response.status, html: await response.text() };
  };

  it('renders a layout registered after the first render', async () => {
    expect((await get('/')).html).toContain('<p>home</p>');
    app.reactLayout('/', {
      layout: {
        default: () => React.createElement('section', { id: 'shell' }, React.createElement(Outlet)),
      },
    });
    expect((await get('/')).html).toContain('<section id="shell">');
  });
});
