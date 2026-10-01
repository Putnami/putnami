import { describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import {
  buildManifest,
  diffManifests,
  formatManifestDiff,
  formatManifestTable,
  hasManifestChanges,
  serializeManifest,
  type WebManifest,
} from '../../src/ssr/manifest';

function sampleManifest(): WebManifest {
  return buildManifest({
    routes: [
      { route: '/tasks', mode: 'ssr', hydration: 'full' },
      { route: '/', mode: 'ssg', hydration: 'islands' },
      { route: '/about', mode: 'ssg', hydration: 'none' },
      { route: '/blog', mode: 'isr', hydration: 'none', revalidate: { seconds: 60 } },
    ],
    islands: [{ id: 'Counter', strategy: 'visible' }],
    hydrateBytes: 50_000,
    islandsBytes: 8000,
  });
}

describe('buildManifest', () => {
  specTest(
    'sorts routes and computes per-route client JS budget',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'render-manifest',
      check: 'the-manifest-is-sorted-and-carries-per-route-client-javascript-bytes',
    },
    () => {
      const m = sampleManifest();
      expect(m.routes.map((r) => r.route)).toEqual(['/', '/about', '/blog', '/tasks']);

      const byRoute = Object.fromEntries(m.routes.map((r) => [r.route, r]));
      expect(byRoute['/'].clientJsBytes).toBe(8000); // islands bundle
      expect(byRoute['/about'].clientJsBytes).toBe(0); // zero JS
      expect(byRoute['/tasks'].clientJsBytes).toBe(50_000); // full hydration
      expect(byRoute['/blog'].revalidate).toEqual({ seconds: 60 });
    },
  );

  it('computes totals', () => {
    const m = sampleManifest();
    expect(m.totals).toEqual({
      routes: 4,
      ssr: 1,
      ssg: 2,
      isr: 1,
      islands: 1,
      maxClientJsBytes: 50_000,
    });
  });

  specTest(
    'serializes deterministically with a trailing newline',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'render-manifest',
      check: 'the-manifest-serializes-deterministically-with-a-trailing-newline',
    },
    () => {
      const a = serializeManifest(sampleManifest());
      const b = serializeManifest(sampleManifest());
      expect(a).toBe(b);
      expect(a.endsWith('\n')).toBe(true);
    },
  );

  specTest(
    'carries the DI-proven-static flag for static routes only',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'render-manifest',
      check: 'the-manifest-carries-the-di-proof-for-static-routes-only',
    },
    () => {
      const m = buildManifest({
        routes: [
          { route: '/', mode: 'ssg', hydration: 'none', diProvenStatic: true },
          { route: '/feed', mode: 'isr', hydration: 'none', diProvenStatic: false },
          { route: '/tasks', mode: 'ssr', hydration: 'full' },
        ],
        islands: [],
        hydrateBytes: 0,
        islandsBytes: 0,
      });
      const byRoute = Object.fromEntries(m.routes.map((r) => [r.route, r]));
      expect(byRoute['/'].diProvenStatic).toBe(true);
      expect(byRoute['/feed'].diProvenStatic).toBe(false);
      expect(byRoute['/tasks'].diProvenStatic).toBeUndefined();
    },
  );
});

describe('formatManifestTable', () => {
  it('renders a header and a row per route', () => {
    const table = formatManifestTable(sampleManifest());
    expect(table).toContain('Route');
    expect(table).toContain('Client JS');
    expect(table).toContain('/tasks');
    expect(table).toContain('SSG');
    expect(table).toContain('islands');
  });
});

describe('diffManifests', () => {
  it('detects added, removed and changed routes', () => {
    const prev = sampleManifest();
    const next = buildManifest({
      routes: [
        { route: '/tasks', mode: 'ssr', hydration: 'full' },
        { route: '/', mode: 'ssr', hydration: 'full' }, // changed: ssg→ssr
        { route: '/new', mode: 'ssg', hydration: 'none' }, // added
        // /about and /blog removed
      ],
      islands: [],
      hydrateBytes: 50_000,
      islandsBytes: 0,
    });

    const diff = diffManifests(prev, next);
    expect(diff.added.map((r) => r.route)).toEqual(['/new']);
    expect(diff.removed.map((r) => r.route).sort()).toEqual(['/about', '/blog']);
    expect(diff.changed.map((c) => c.route)).toEqual(['/']);
    expect(hasManifestChanges(diff)).toBe(true);
  });

  it('reports no changes for identical manifests', () => {
    const diff = diffManifests(sampleManifest(), sampleManifest());
    expect(hasManifestChanges(diff)).toBe(false);
    expect(formatManifestDiff(diff)).toBe('No web rendering changes.');
  });

  it('formats a readable diff', () => {
    const prev = sampleManifest();
    const next = buildManifest({
      routes: [{ route: '/', mode: 'ssr', hydration: 'full' }],
      islands: [],
      hydrateBytes: 50_000,
      islandsBytes: 0,
    });
    const text = formatManifestDiff(diffManifests(prev, next));
    expect(text).toContain('mode ssg → ssr');
    expect(text).toContain('hydration islands → full');
  });

  it('detects and formats a DI-proven-static regression', () => {
    const base = (diProvenStatic: boolean): WebManifest =>
      buildManifest({
        routes: [{ route: '/blog', mode: 'ssg', hydration: 'none', diProvenStatic }],
        islands: [],
        hydrateBytes: 0,
        islandsBytes: 0,
      });

    const diff = diffManifests(base(true), base(false));
    expect(diff.changed.map((c) => c.route)).toEqual(['/blog']);
    expect(formatManifestDiff(diff)).toContain('di-proven proven → hybrid');
  });
});
