import { afterAll, beforeAll, describe, expect, it } from 'bun:test';
import { application, HttpPlugin } from '@putnami/application';
import { specTest } from '@putnami/runtime/spectest';
import { resolve } from 'node:path';
import React from 'react';
import type { RouteObject } from 'react-router';
import { Link } from '../../src/client/components/link';
import { LocaleSwitcher } from '../../src/client/i18n/locale-switcher';
import { localizePath, parseLocaleSettings, splitLocalePath } from '../../src/shared/i18n/locale';
import { messages, useT } from '../../src/shared/i18n/messages';
import { localeRoutes } from '../../src/shared/i18n/route-aliases';
import { localizeRows, translatedPathErrors, translatedRedirectRows } from '../../src/ssr/i18n/build';
import { buildSitemap } from '../../src/ssr/i18n/sitemap';
import { loader as loaderBuilder } from '../../src/ssr/loader';
import { ReactApplication } from '../../src/ssr/react-application';
import { createMockReactConfig } from '../fixtures/route.fixtures';

const feature = 'typescript/web-localization';

const raw = {
  locales: ['en', 'fr'],
  defaultLocale: 'en',
  siteUrl: 'https://example.com',
  exclude: ['/docs/**'],
  paths: { fr: { '/pricing': '/tarifs' } },
};
const settings = parseLocaleSettings(raw);

const home = messages('home', { title: 'Pricing' });

function Pricing() {
  return React.createElement(
    'main',
    null,
    React.createElement('h1', null, useT(home).title()),
    React.createElement(Link, { to: '/pricing?plan=team' }, 'self'),
    React.createElement(LocaleSwitcher, { label: 'Language' }),
  );
}

describe('translated paths', () => {
  const packageRoot = resolve(import.meta.dir, '..', '..');
  let server: { stop: () => Promise<void> };
  let origin = '';

  beforeAll(async () => {
    process.env['PUTNAMI_WORKSPACE_ROOT'] = resolve(packageRoot, '..', '..', '..');
    process.env['PUTNAMI_PROJECT_ROOT'] = packageRoot;
    const root = new HttpPlugin({ port: 0, secure: false });
    const app = new ReactApplication(createMockReactConfig({ isDevelopment: true }), new HttpPlugin());
    app
      .i18n({ settings: raw, messages: { fr: { home: { title: 'Tarifs' } } } })
      .reactPage('/', { page: { default: () => React.createElement('p', null, 'home') } })
      .reactPage('/pricing', {
        page: { default: Pricing },
        loader: { loader: loaderBuilder(() => ({ plans: 3 })) },
      });
    root.merge(app.getHttpPlugin());
    const composed = application().use(root);
    await composed.start();
    server = composed;
    origin = `http://localhost:${root.getServer()?.port}`;
  });

  afterAll(async () => {
    await server.stop();
  });

  const get = (path: string, accept = 'text/html') =>
    fetch(`${origin}${path}`, { headers: { accept }, redirect: 'manual' });

  specTest(
    'serves a page at its translated path and redirects its untranslated one',
    {
      feature,
      requirement: 'translated-paths',
      check: 'a-translated-path-serves-the-page-and-the-untranslated-one-redirects',
    },
    async () => {
      const page = await get('/fr/tarifs');
      expect(page.status).toBe(200);
      expect(await page.text()).toContain('<h1>Tarifs</h1>');
      const moved = await get('/fr/pricing?plan=team');
      expect(moved.status).toBe(308);
      expect(moved.headers.get('location')).toBe('/fr/tarifs?plan=team');
      expect((await get('/pricing')).status).toBe(200);
      expect((await get('/tarifs')).status).toBe(404);
      const data = await get('/fr/tarifs.json', 'application/json');
      expect(data.status).toBe(200);
      expect(await data.json()).toMatchObject({ plans: 3 });
    },
  );

  specTest(
    'links, alternates, the switcher and the sitemap use the translated path',
    { feature, requirement: 'translated-paths', check: 'links-alternates-and-the-switcher-use-the-translated-path' },
    async () => {
      const french = await (await get('/fr/tarifs')).text();
      expect(french).toMatch(/<a href="\/fr\/tarifs\?plan=team"[^>]*>self<\/a>/);
      expect(french).toContain('href="https://example.com/fr/tarifs"');
      expect(french).toContain('href="/fr/tarifs?locale=fr" hrefLang="fr"');
      expect(french).toContain('href="/pricing?locale=en" hrefLang="en"');
      const english = await (await get('/pricing')).text();
      expect(english).toMatch(/<a href="\/pricing\?plan=team"[^>]*>self<\/a>/);
      expect(english).toContain('href="/fr/tarifs?locale=fr" hrefLang="fr"');
      expect(buildSitemap(settings, [{ route: '/pricing', prerendered: [] }])).toContain(
        '<loc>https://example.com/fr/tarifs</loc>',
      );
    },
  );
});

describe('translated path settings', () => {
  const invalid = (paths: unknown) => () => parseLocaleSettings({ ...raw, paths });

  specTest(
    'refuses a translated path that names a locale, collides or leaves the localized set',
    { feature, requirement: 'translated-paths', check: 'an-invalid-translated-path-fails-generation' },
    () => {
      expect(invalid({ fr: { '/pricing': '/fr/tarifs' } })).toThrow("'/fr/tarifs' names a locale");
      expect(invalid({ fr: { '/a': '/x', '/b': '/x' } })).toThrow("'/a' and '/b' both translate to '/x'");
      expect(invalid({ fr: { '/pricing': '/docs/tarifs' } })).toThrow('must both be localized');
      expect(invalid({ fr: { '/blog/:slug': '/blogue/:slug' } })).toThrow('a route with parameters');
      expect(invalid({ en: { '/pricing': '/prices' } })).toThrow('is not a published locale other than the default');
      expect(invalid({ fr: { '/': '/accueil' } })).toThrow("the locale's home is not translated");
      expect(invalid({ fr: ['/x'] })).toThrow('must map a route to its path in fr');
      expect(
        translatedPathErrors(
          parseLocaleSettings({
            ...raw,
            paths: { fr: { '/missing': '/absent', '/pricing': '/about', '/blog/post': '/billet' } },
          }),
          ['/pricing', '/about', '/blog/post'],
          ['/', '/blog'],
        ),
      ).toEqual([
        "i18n.paths.fr['/missing']: no page is served at '/missing'",
        "i18n.paths.fr['/pricing']: '/about' is the path of another page",
        "i18n.paths.fr['/blog/post']: '/billet' must stay under '/blog', the layout of '/blog/post'",
      ]);
      expect(translatedPathErrors(settings, ['/pricing'], ['/'])).toEqual([]);
    },
  );

  it('reads a translated path as the route it translates', () => {
    expect(splitLocalePath(settings, '/fr/tarifs')).toEqual({ locale: 'fr', path: '/pricing' });
    expect(splitLocalePath(settings, '/fr/pricing')).toEqual({ locale: 'fr', path: '/pricing' });
    expect(localizePath(settings, '/pricing', 'fr')).toBe('/fr/tarifs');
    expect(localizePath(settings, '/pricing', 'en')).toBe('/pricing');
  });

  it('adds the translated path beside its page in the locale route tree only', () => {
    const page: RouteObject = { id: 'root-pricing-page', path: 'pricing' };
    const routes: RouteObject[] = [{ id: 'root', path: '/', children: [page] }];
    expect(localeRoutes(routes, settings, 'en')).toBe(routes);
    const french = localeRoutes(routes, settings, 'fr');
    expect(french[0]?.children?.map((r) => [r.id, r.path])).toEqual([
      ['root-pricing-page', 'pricing'],
      ['root-pricing-page@fr', 'tarifs'],
    ]);
    expect(routes[0]?.children).toHaveLength(1);
  });

  it('lists the translated path and its redirect in the route inventory', () => {
    const row = { route: '/pricing', evidencePath: 'src/app/pricing/page.tsx', hasAction: false };
    expect(localizeRows(settings, [row]).map((r) => r.route)).toEqual(['/pricing', '/fr/tarifs']);
    expect(translatedRedirectRows(settings, [row])).toEqual([{ ...row, route: '/fr/pricing' }]);
  });
});
