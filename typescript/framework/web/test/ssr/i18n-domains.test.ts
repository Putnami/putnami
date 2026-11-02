import { afterAll, beforeAll, describe, expect, it } from 'bun:test';
import { application, HttpPlugin } from '@putnami/application';
import { specTest } from '@putnami/runtime/spectest';
import { resolve } from 'node:path';
import React from 'react';
import { Link } from '../../src/client/components/link';
import { LocaleSwitcher } from '../../src/client/i18n/locale-switcher';
import { localeHref, localizedUrl, parseLocaleSettings } from '../../src/shared/i18n/locale';
import { messages, useT } from '../../src/shared/i18n/messages';
import { buildSitemap } from '../../src/ssr/i18n/sitemap';
import { loader as loaderBuilder } from '../../src/ssr/loader';
import { ReactApplication } from '../../src/ssr/react-application';
import { page } from '../../src/ssr/page';
import { renderStaticDocument } from '../../src/ssr/static-render';
import { createMockReactConfig } from '../fixtures/route.fixtures';

const feature = 'typescript/web-localization';
const requirement = 'locale-domains';

const raw = {
  locales: ['en', 'fr'],
  defaultLocale: 'en',
  siteUrl: 'https://example.com',
  exclude: ['/docs/**'],
  paths: { fr: { '/pricing': '/tarifs' } },
  domains: { fr: 'https://example.fr' },
};
const settings = parseLocaleSettings(raw);

const home = messages('home', { title: 'Pricing' });

function Pricing() {
  return React.createElement(
    'main',
    null,
    React.createElement('h1', null, useT(home).title()),
    React.createElement(Link, { to: '/pricing?plan=team' }, 'self'),
    React.createElement(Link, { to: '/docs/intro' }, 'docs'),
    React.createElement(LocaleSwitcher, { label: 'Language' }),
  );
}

describe('locale domains', () => {
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
      .reactPage('/docs/intro', { page: { default: () => React.createElement('p', null, 'docs') } })
      .reactPage('/plans', { page: { default: page().cache({ ttl: 60_000 }).render(Pricing) } })
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

  const get = (path: string, host?: string, headers: Record<string, string> = {}) =>
    fetch(`${origin}${path}`, {
      headers: { accept: 'text/html', ...headers, ...(host ? { host } : {}) },
      redirect: 'manual',
    });

  specTest(
    'serves a request to a locale domain in its locale',
    { feature, requirement, check: 'a-request-to-a-locale-domain-is-served-in-its-locale' },
    async () => {
      const page = await get('/tarifs', 'example.fr');
      expect(page.status).toBe(200);
      expect(page.headers.get('content-language')).toBe('fr');
      expect(await page.text()).toContain('<h1>Tarifs</h1>');
      expect(await (await get('/', 'example.fr')).text()).toContain('lang="fr"');
      const data = await get('/tarifs.json', 'example.fr', { accept: 'application/json' });
      expect(await data.json()).toMatchObject({ plans: 3 });
      const moved = await get('/pricing?plan=team', 'example.fr');
      expect(moved.status).toBe(308);
      expect(moved.headers.get('location')).toBe('/tarifs?plan=team');
      // The locale's prefix on its own domain names the same page: it moves to the domain's root.
      const prefixed = await get('/tarifs?plan=team', 'example.fr');
      expect(prefixed.status).toBe(200);
      const doubled = await get('/fr/tarifs?plan=team', 'example.fr');
      expect(doubled.status).toBe(308);
      expect(doubled.headers.get('location')).toBe('https://example.fr/tarifs?plan=team');
      const excluded = await get('/docs/intro', 'example.fr');
      expect(excluded.status).toBe(308);
      expect(excluded.headers.get('location')).toBe('https://example.com/docs/intro');
      // The site keeps serving the default locale.
      expect(await (await get('/pricing', 'example.com')).text()).toContain('<h1>Pricing</h1>');
    },
  );

  specTest(
    'builds the URLs of a domain locale on its origin, without the prefix',
    { feature, requirement, check: 'the-urls-of-a-domain-locale-use-its-origin' },
    async () => {
      const french = await (await get('/tarifs', 'example.fr')).text();
      expect(french).toContain('<link rel="canonical" href="https://example.fr/tarifs"');
      expect(french).toContain('<link rel="alternate" hreflang="en" href="https://example.com/pricing"');
      expect(french).toMatch(/<a href="\/tarifs\?plan=team"[^>]*>self<\/a>/);
      expect(french).toMatch(/<a href="https:\/\/example.com\/docs\/intro"[^>]*>docs<\/a>/);
      expect(french).toContain('href="https://example.com/pricing?locale=en" hrefLang="en"');
      expect(french).toContain('href="/tarifs?locale=fr" hrefLang="fr"');
      const english = await (await get('/pricing', 'example.com')).text();
      expect(english).toContain('href="https://example.fr/tarifs?locale=fr" hrefLang="fr"');
      expect(english).toContain('href="/pricing?locale=en" hrefLang="en"');
      expect(buildSitemap(settings, [{ route: '/pricing', prerendered: [] }])).toContain(
        '<loc>https://example.fr/tarifs</loc>',
      );
      const built = await renderStaticDocument({
        routes: [{ path: '/', children: [{ id: 'pricing', path: 'pricing', element: React.createElement(Pricing) }] }],
        route: '/pricing',
        pathname: '/fr/tarifs',
        i18n: { settings, messages: { fr: { home: { title: 'Tarifs' } } } },
      });
      expect(built.html).toContain('<h1>Tarifs</h1>');
      expect(built.html).toMatch(/<a href="\/tarifs\?plan=team"[^>]*>self<\/a>/);
    },
  );

  specTest(
    'sends the prefix on the site to the domain, and serves it on any other host',
    { feature, requirement, check: 'the-prefix-on-the-site-redirects-to-the-domain' },
    async () => {
      const moved = await get('/fr/tarifs?plan=team', 'example.com');
      expect(moved.status).toBe(308);
      expect(moved.headers.get('location')).toBe('https://example.fr/tarifs?plan=team');
      const entry = await get('/', 'example.com', { 'accept-language': 'fr' });
      expect(entry.status).toBe(302);
      expect(entry.headers.get('location')).toBe('https://example.fr/');
      // A development server keeps every locale under its prefix.
      expect((await get('/', undefined, { 'accept-language': 'fr' })).headers.get('location')).toBe('/fr');
      // A cached page keeps one copy per origin: the prefix host's basename never reaches the domain.
      expect(await (await get('/fr/plans')).text()).toContain('window.__basename="/fr"');
      expect(await (await get('/plans', 'example.fr')).text()).not.toContain('window.__basename');
      const local = await get('/fr/tarifs');
      expect(local.status).toBe(200);
      const text = await local.text();
      expect(text).toContain('<h1>Tarifs</h1>');
      expect(text).toContain('href="/pricing?locale=en" hrefLang="en"');
      expect(text).toContain('href="/fr/tarifs?locale=fr" hrefLang="fr"');
    },
  );
});

describe('locale domain settings', () => {
  const invalid = (domains: unknown) => () => parseLocaleSettings({ ...raw, domains });

  it('refuses a domain that is not an origin, the site, shared, or for the default locale', () => {
    expect(invalid({ fr: 'https://example.fr/fr' })).toThrow('must be an http or https origin');
    expect(invalid({ fr: 'example.fr' })).toThrow('must be an http or https origin');
    expect(invalid({ fr: 'https://example.com' })).toThrow("'example.com' is already the host of i18n.siteUrl");
    expect(invalid({ en: 'https://example.us' })).toThrow('is not a published locale other than the default');
    expect(() =>
      parseLocaleSettings({
        ...raw,
        locales: ['en', 'fr', 'de'],
        domains: { fr: 'https://x.eu', de: 'https://x.eu/' },
      }),
    ).toThrow("i18n.domains.de: 'x.eu' is already the host of i18n.domains.fr");
    expect(parseLocaleSettings({ ...raw, domains: { fr: 'https://example.fr/' } }).domains).toEqual({
      fr: 'https://example.fr',
    });
  });

  it('builds an href on the origin that serves the target', () => {
    const onSite = { locale: 'en', served: 'site' } as const;
    const onDomain = { locale: 'fr', served: 'domain' } as const;
    expect(localeHref(settings, '/pricing', 'fr', onSite)).toBe('https://example.fr/tarifs');
    expect(localeHref(settings, '/pricing', 'fr', onDomain)).toBe('/tarifs');
    expect(localeHref(settings, '/', 'fr', onDomain)).toBe('/');
    expect(localeHref(settings, '/pricing', 'en', onDomain)).toBe('https://example.com/pricing');
    expect(localeHref(settings, '/docs/intro', 'fr', onDomain)).toBe('https://example.com/docs/intro');
    expect(localeHref(settings, '/pricing', 'fr', { locale: 'en', served: 'prefix' })).toBe('/fr/tarifs');
    expect(localizedUrl(settings, '/', 'fr')).toBe('https://example.fr/');
  });
});
