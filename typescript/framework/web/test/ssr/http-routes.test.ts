import { describe, expect, it } from 'bun:test';
import { buildWebAssetHttpRoutes, buildWebPageHttpRoutes } from '../../src/ssr/http-routes';

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
