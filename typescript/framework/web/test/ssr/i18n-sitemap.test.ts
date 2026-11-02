import { describe, expect, it } from 'bun:test';
import type { HttpResponse } from '@putnami/application';
import { specTest } from '@putnami/runtime/spectest';
import { mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { parseLocaleSettings } from '../../src/shared/i18n/locale';
import { buildWebSitemapHttpRoutes } from '../../src/ssr/http-routes';
import { buildSitemap, sitemapHandler } from '../../src/ssr/i18n/sitemap';

const feature = 'typescript/web-localization';

const settings = parseLocaleSettings({
  locales: ['en', 'fr'],
  defaultLocale: 'en',
  siteUrl: 'https://example.com',
  exclude: ['/docs/**'],
});

/** The `<url>` blocks of a sitemap, by their `<loc>`. */
function urls(xml: string): Map<string, string> {
  const blocks = new Map<string, string>();
  for (const [block] of xml.matchAll(/<url>[\s\S]*?<\/url>/g)) {
    const loc = /<loc>(.*?)<\/loc>/.exec(block)?.[1];
    if (loc) blocks.set(loc, block);
  }
  return blocks;
}

describe('sitemap', () => {
  specTest(
    'lists each page URL in each locale with its alternates',
    { feature, requirement: 'sitemap', check: 'the-sitemap-lists-each-url-with-its-alternates' },
    () => {
      const xml = buildSitemap(settings, [
        { route: '/', prerendered: [] },
        { route: '/pricing', prerendered: [] },
        { route: '/docs/*', prerendered: ['/docs/intro'] },
      ]);
      expect(xml).toStartWith('<?xml version="1.0" encoding="UTF-8"?>');
      expect(xml).toContain('xmlns:xhtml="http://www.w3.org/1999/xhtml"');
      const blocks = urls(xml);
      expect([...blocks.keys()]).toEqual([
        'https://example.com/',
        'https://example.com/docs/intro',
        'https://example.com/fr',
        'https://example.com/fr/pricing',
        'https://example.com/pricing',
      ]);
      for (const loc of ['https://example.com/fr/pricing', 'https://example.com/pricing']) {
        const block = blocks.get(loc) ?? '';
        expect(block).toContain('hreflang="en" href="https://example.com/pricing"');
        expect(block).toContain('hreflang="fr" href="https://example.com/fr/pricing"');
        expect(block).toContain('hreflang="x-default" href="https://example.com/pricing"');
      }
      // A page served in the default locale only has no alternates.
      expect(blocks.get('https://example.com/docs/intro')).not.toContain('xhtml:link');
    },
  );

  specTest(
    'leaves out a route with parameters and no pre-rendered path',
    { feature, requirement: 'sitemap', check: 'a-route-with-parameters-and-no-prerendered-path-is-left-out' },
    () => {
      const xml = buildSitemap(settings, [
        { route: '/orders/:id', prerendered: [] },
        { route: '/blog/:slug', prerendered: ['/blog/hello', '/fr/blog/hello'] },
      ]);
      expect([...urls(xml).keys()]).toEqual(['https://example.com/blog/hello', 'https://example.com/fr/blog/hello']);
    },
  );

  it('writes the same bytes for the same pages in any order', () => {
    const pages = [
      { route: '/b', prerendered: [] },
      { route: '/a', prerendered: [] },
    ];
    expect(buildSitemap(settings, pages)).toBe(buildSitemap(settings, [...pages].reverse()));
  });

  it('escapes a URL for XML', () => {
    expect(buildSitemap(settings, [{ route: '/a&b', prerendered: [] }])).toContain(
      '<loc>https://example.com/a&amp;b</loc>',
    );
  });

  it('serves the file the build wrote, and 404 before', async () => {
    const dir = mkdtempSync(join(tmpdir(), 'putnami-sitemap-'));
    const path = join(dir, 'sitemap.xml');
    const serve = sitemapHandler(path);
    const ctx = {} as Parameters<typeof serve>[0];
    expect(((await serve(ctx)) as HttpResponse).status).toBe(404);
    writeFileSync(path, '<urlset/>');
    const served = ((await serve(ctx)) as HttpResponse).get();
    expect(served.headers.get('content-type')).toBe('application/xml; charset=utf-8');
    expect(await served.text()).toBe('<urlset/>');
  });

  it('reports the sitemap route to the HTTP route inventory', () => {
    expect(buildWebSitemapHttpRoutes('public')).toEqual([
      {
        match: 'exact',
        path: '/sitemap.xml',
        methods: ['GET', 'HEAD'],
        publicEdge: true,
        provenance: { package: '@putnami/web', sourceKind: 'public-file', evidencePath: '.gen/public/sitemap.xml' },
      },
    ]);
  });
});
