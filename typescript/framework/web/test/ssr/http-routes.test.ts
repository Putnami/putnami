import { describe, expect, it } from 'bun:test';
import {
  canonicalizeHttpRoutes,
  type GeneratedHttpRoute,
  type HttpRoute,
  parseAndValidateHttpRoutes,
  serializeCanonicalHttpRoutes,
} from '@putnami/application/http-routes';
import { buildWebAssetHttpRoutes, buildWebPageHttpRoutes } from '../../src/ssr/http-routes';

/**
 * Stamp project identity as writeHttpRoutesFragment does, serialize the
 * canonical putnami.http-routes.v1 bytes, and return the routes the strict
 * parser reads back from those bytes.
 */
function committedRoutes(routes: readonly GeneratedHttpRoute[]): HttpRoute[] {
  const canonical = canonicalizeHttpRoutes(
    routes.map((route) => ({ ...route, provenance: { project: 'example/web', ...route.provenance } })),
  );
  expect(canonical.diagnostics).toEqual([]);
  if (!canonical.manifest) throw new Error('no canonical inventory');
  const serialized = serializeCanonicalHttpRoutes({ routes: canonical.manifest.routes });
  if (serialized.text === undefined) throw new Error('no canonical bytes');
  const parsed = parseAndValidateHttpRoutes(serialized.text);
  expect(parsed.diagnostics).toEqual([]);
  if (!parsed.manifest) throw new Error('the strict parser rejected the canonical bytes');
  return parsed.manifest.routes;
}

/**
 * The page and action rule of protocols/http-routes: a file-route with GET and
 * HEAD is a page, a file-route with POST at the same match and path as a page
 * is its action, and page files are the distinct evidence paths of pages.
 */
function classify(routes: readonly HttpRoute[]) {
  const key = (route: HttpRoute) => `${route.match} ${route.path}`;
  const fileRoutes = routes.filter((route) => route.provenance.sourceKind === 'file-route');
  const pages = fileRoutes.filter((route) => route.methods.join(',') === 'GET,HEAD');
  const pageKeys = new Set(pages.map(key));
  const actions = fileRoutes.filter((route) => route.methods.join(',') === 'POST' && pageKeys.has(key(route)));
  return { pages, actions, pageFiles: new Set(pages.map((route) => route.provenance.evidencePath)) };
}

describe('web HTTP route generation', () => {
  it('emits parameterized pages and actions with file provenance', () => {
    const routes = buildWebPageHttpRoutes([
      {
        route: '/blog/:slug',
        evidencePath: 'src/app/blog/[slug]/page.tsx',
        hasAction: true,
      },
    ]);

    expect(routes).toEqual([
      expect.objectContaining({ match: 'template', path: '/blog/{slug}', methods: ['GET', 'HEAD'] }),
      expect.objectContaining({
        match: 'template',
        path: '/blog/{slug}',
        methods: ['POST'],
        provenance: expect.objectContaining({ evidencePath: 'src/app/blog/[slug]/action.ts' }),
      }),
    ]);
  });

  it('writes slash-separated page and action provenance from a Windows-separated scan path', () => {
    const routes = buildWebPageHttpRoutes([
      { route: '/users', evidencePath: 'src\\app\\users\\page.tsx', hasAction: true },
    ]);

    expect(routes.map((route) => route.provenance?.evidencePath)).toEqual([
      'src/app/users/page.tsx',
      'src/app/users/action.ts',
    ]);
  });

  it('expands a finite static catch-all into deterministic exact routes', () => {
    const routes = buildWebPageHttpRoutes([
      {
        route: '/docs/*',
        evidencePath: 'src/app/docs/[...page]/page.tsx',
        hasAction: false,
        expandedPaths: ['/docs/reference/config', '/docs/getting-started', '/docs/getting-started'],
      },
    ]);

    expect(routes).toEqual([
      expect.objectContaining({ match: 'exact', path: '/docs/getting-started', methods: ['GET', 'HEAD'] }),
      expect.objectContaining({ match: 'exact', path: '/docs/reference/config', methods: ['GET', 'HEAD'] }),
    ]);
  });

  it('keeps an unexpanded catch-all unsupported for the shared validator', () => {
    expect(
      buildWebPageHttpRoutes([
        {
          route: '/docs/*',
          evidencePath: 'src/app/docs/[...page]/page.tsx',
          hasAction: false,
        },
      ])[0],
    ).toEqual(expect.objectContaining({ path: '/docs/*' }));
  });

  it('commits a page and its action as one GET,HEAD and one POST file-route at the same path', () => {
    const routes = committedRoutes([
      ...buildWebPageHttpRoutes([
        { route: '/', evidencePath: 'src/app/page.tsx', hasAction: false },
        { route: '/blog/:slug', evidencePath: 'src/app/blog/[slug]/page.tsx', hasAction: true },
      ]),
      ...buildWebAssetHttpRoutes('public', ['/react/hydrate.ABC123.js']),
    ]);

    const blog = routes.filter((route) => route.path === '/blog/{slug}');
    expect(blog).toEqual([
      expect.objectContaining({
        match: 'template',
        methods: ['GET', 'HEAD'],
        provenance: expect.objectContaining({
          sourceKind: 'file-route',
          evidencePath: 'src/app/blog/[slug]/page.tsx',
        }),
      }),
      expect.objectContaining({
        match: 'template',
        methods: ['POST'],
        provenance: expect.objectContaining({
          sourceKind: 'file-route',
          evidencePath: 'src/app/blog/[slug]/action.ts',
        }),
      }),
    ]);

    const { pages, actions, pageFiles } = classify(routes);
    expect(pages.map((route) => route.path)).toEqual(['/', '/blog/{slug}']);
    expect(actions.map((route) => [route.path, route.provenance.evidencePath])).toEqual([
      ['/blog/{slug}', 'src/app/blog/[slug]/action.ts'],
    ]);
    expect([...pageFiles]).toEqual(['src/app/page.tsx', 'src/app/blog/[slug]/page.tsx']);
    // The hashed-asset prefix serves GET and HEAD too, and is no page.
    expect(routes).toContainEqual(
      expect.objectContaining({
        match: 'prefix',
        path: '/react/',
        methods: ['GET', 'HEAD'],
        provenance: expect.objectContaining({ sourceKind: 'static-mount' }),
      }),
    );
  });

  it('counts one page file for every route a static expansion produces', () => {
    const routes = committedRoutes(
      buildWebPageHttpRoutes([
        {
          route: '/docs/*',
          evidencePath: 'src/app/docs/[...page]/page.tsx',
          hasAction: true,
          expandedPaths: ['/docs/getting-started', '/docs/reference/config', '/docs/reference/cli'],
        },
      ]),
    );

    const { pages, actions, pageFiles } = classify(routes);
    expect(pages.map((route) => route.path)).toEqual([
      '/docs/getting-started',
      '/docs/reference/cli',
      '/docs/reference/config',
    ]);
    expect(pageFiles).toEqual(new Set(['src/app/docs/[...page]/page.tsx']));
    expect(actions.map((route) => route.path)).toEqual(pages.map((route) => route.path));
    expect(new Set(actions.map((route) => route.provenance.evidencePath))).toEqual(
      new Set(['src/app/docs/[...page]/action.ts']),
    );
  });

  it('collapses hashed build assets to bounded prefixes', () => {
    expect(buildWebAssetHttpRoutes('public', ['/react/hydrate.ABC123.js', '/react/chunk.DEF456.js'])).toEqual([
      expect.objectContaining({
        match: 'prefix',
        path: '/react/',
        methods: ['GET', 'HEAD'],
        provenance: expect.objectContaining({ sourceKind: 'static-mount', evidencePath: '.gen/public/react' }),
      }),
    ]);
  });
});
