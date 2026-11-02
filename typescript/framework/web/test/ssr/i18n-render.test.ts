import { afterAll, beforeAll, describe, expect, it } from 'bun:test';
import { application, HttpPlugin } from '@putnami/application';
import { specTest } from '@putnami/runtime/spectest';
import { resolve } from 'node:path';
import React from 'react';
import type { RouteObject } from 'react-router';
import { Link } from '../../src/client/components/link';
import { LocaleSwitcher } from '../../src/client/i18n/locale-switcher';
import { island } from '../../src/client/island/island';
import { parseLocaleSettings } from '../../src/shared/i18n/locale';
import { messages, useT } from '../../src/shared/i18n/messages';
import { I18N_PAYLOAD_ID } from '../../src/shared/i18n/payload';
import { excludedRedirectRows, localizeRows } from '../../src/ssr/i18n/build';
import { applyLocaleHead, type I18nRuntimeOptions } from '../../src/ssr/i18n/runtime';
import { ReactApplication } from '../../src/ssr/react-application';
import { renderStaticDocument } from '../../src/ssr/static-render';
import { createMockReactConfig } from '../fixtures/route.fixtures';

const feature = 'typescript/web-localization';

const settings = parseLocaleSettings({
  locales: ['en', 'fr', 'ar'],
  defaultLocale: 'en',
  siteUrl: 'https://example.com',
  exclude: ['/docs/**'],
});

const home = messages('home', { title: 'Pricing' });
const footer = messages('footer', { note: 'Made in Paris' });
const _unused = messages('unused', { never: 'Never rendered' });

const i18n: I18nRuntimeOptions = {
  settings,
  messages: {
    fr: {
      home: { title: 'Tarifs' },
      footer: { note: 'Fait à Paris' },
      unused: { never: 'Jamais affiché' },
    },
    ar: { home: { title: 'الأسعار' } },
  },
};

function Pricing() {
  const t = useT(home);
  return React.createElement(
    'main',
    null,
    React.createElement('h1', null, t.title()),
    React.createElement(Link, { to: '/pricing' }, 'pricing'),
    React.createElement(Link, { to: '/docs/intro' }, 'docs'),
    React.createElement(Link, { to: { pathname: '/docs/intro', hash: '#a' } }, 'docs-object'),
    React.createElement(Link, { to: '/ar/pricing' }, 'arabic'),
    React.createElement(LocaleSwitcher, { label: 'Language' }),
  );
}

function Docs() {
  return React.createElement('main', null, 'docs');
}

function readPayload(html: string): { locale: string; messages: Record<string, unknown> } | undefined {
  const match = new RegExp(`<script type="application/json" id="${I18N_PAYLOAD_ID}">(.*?)</script>`).exec(html);
  return match?.[1] ? JSON.parse(match[1]) : undefined;
}

describe('localized server render', () => {
  const packageRoot = resolve(import.meta.dir, '..', '..');
  let server: { stop: () => Promise<void> };
  let origin = '';

  beforeAll(async () => {
    process.env['PUTNAMI_WORKSPACE_ROOT'] = resolve(packageRoot, '..', '..', '..');
    process.env['PUTNAMI_PROJECT_ROOT'] = packageRoot;
    const root = new HttpPlugin({ port: 0, secure: false });
    const app = new ReactApplication(createMockReactConfig({ isDevelopment: true }), new HttpPlugin());
    app
      .i18n(i18n)
      .reactPage('/', { page: { default: () => React.createElement('p', null, 'home') } })
      .reactPage('/pricing', { page: { default: Pricing } })
      .reactPage('/docs/intro', { page: { default: Docs } });
    root.merge(app.getHttpPlugin());
    const composed = application().use(root);
    await composed.start();
    server = composed;
    origin = `http://localhost:${root.getServer()?.port}`;
  });

  afterAll(async () => {
    await server.stop();
  });

  const get = (path: string, headers: Record<string, string> = {}) =>
    fetch(`${origin}${path}`, { headers: { accept: 'text/html', ...headers }, redirect: 'manual' });

  specTest(
    'registers each route once per published locale',
    { feature, requirement: 'locale-routes', check: 'each-route-is-registered-once-per-published-locale' },
    async () => {
      for (const path of ['/pricing', '/fr/pricing', '/ar/pricing']) {
        const response = await get(path);
        expect(response.status).toBe(200);
      }
      expect(await (await get('/fr/pricing')).text()).toContain('<h1>Tarifs</h1>');
      expect((await get('/de/pricing')).status).toBe(404);
    },
  );

  specTest(
    'keeps an excluded route in the default locale only',
    { feature, requirement: 'locale-routes', check: 'an-excluded-route-stays-in-the-default-locale-only' },
    async () => {
      expect((await get('/docs/intro')).status).toBe(200);
      expect((await get('/fr/docs/intro')).status).toBe(308);
    },
  );

  specTest(
    'redirects an excluded route under a locale prefix to its one URL',
    { feature, requirement: 'excluded-route-redirect', check: 'an-excluded-route-under-a-prefix-redirects' },
    async () => {
      for (const path of ['/fr/docs/intro?q=1', '/ar/docs/intro?q=1']) {
        const response = await get(path);
        expect(response.status).toBe(308);
        expect(response.headers.get('location')).toBe('/docs/intro?q=1');
      }
      expect((await get('/fr/docs/missing')).status).toBe(404);
    },
  );

  specTest(
    'writes the locale and its direction on the html element',
    { feature, requirement: 'locale-document', check: 'the-html-element-carries-the-route-locale-and-direction' },
    async () => {
      expect(await (await get('/fr/pricing')).text()).toContain('<html lang="fr" dir="ltr">');
      expect(await (await get('/ar/pricing')).text()).toContain('<html lang="ar" dir="rtl">');
      expect(await (await get('/pricing')).text()).toContain('<html lang="en" dir="ltr">');
    },
  );

  specTest(
    'sends Content-Language on a per-request page',
    { feature, requirement: 'locale-document', check: 'a-per-request-page-sends-content-language' },
    async () => {
      expect((await get('/fr/pricing')).headers.get('content-language')).toBe('fr');
      expect((await get('/pricing')).headers.get('content-language')).toBe('en');
    },
  );

  specTest(
    'ships the whole catalog of its locale to a hydrated page',
    { feature, requirement: 'message-payload', check: 'a-hydrated-page-ships-its-locale-catalog' },
    async () => {
      const payload = readPayload(await (await get('/fr/pricing')).text());
      expect(payload?.locale).toBe('fr');
      expect(Object.keys(payload?.messages ?? {})).toEqual(['footer', 'home', 'unused']);
      expect(readPayload(await (await get('/ar/pricing')).text())?.messages).toEqual({
        home: { title: 'الأسعار' },
      });
    },
  );

  specTest(
    'ships no catalog in the default locale',
    { feature, requirement: 'message-payload', check: 'the-default-locale-ships-no-catalog' },
    async () => {
      const payload = readPayload(await (await get('/pricing')).text());
      expect(payload?.locale).toBe('en');
      expect(payload?.messages).toEqual({});
    },
  );

  specTest(
    'lists absolute alternates for every locale and x-default',
    { feature, requirement: 'alternate-links', check: 'a-localized-page-lists-absolute-alternates-and-x-default' },
    async () => {
      const html = await (await get('/fr/pricing')).text();
      expect(html).toContain('<link rel="canonical" href="https://example.com/fr/pricing"');
      expect(html).toContain('<link rel="alternate" hreflang="en" href="https://example.com/pricing"');
      expect(html).toContain('<link rel="alternate" hreflang="fr" href="https://example.com/fr/pricing"');
      expect(html).toContain('<link rel="alternate" hreflang="ar" href="https://example.com/ar/pricing"');
      expect(html).toContain('<link rel="alternate" hreflang="x-default" href="https://example.com/pricing"');
      expect(html).toContain('<meta property="og:locale" content="fr"');
      expect(html).toContain('<meta property="og:locale:alternate" content="en"');
      expect(html).toContain('<meta property="og:locale:alternate" content="ar"');
    },
  );

  specTest(
    'lists no alternates for a route outside the localized set',
    { feature, requirement: 'alternate-links', check: 'a-route-outside-the-localized-set-has-no-alternates' },
    async () => {
      const html = await (await get('/docs/intro')).text();
      expect(html).toContain('<link rel="canonical" href="https://example.com/docs/intro"');
      expect(html).not.toContain('hreflang=');
      expect(html).not.toContain('og:locale');
    },
  );

  specTest(
    'keeps the locale prefix on an absolute link',
    { feature, requirement: 'localized-links', check: 'an-absolute-link-keeps-the-current-locale-prefix' },
    async () => {
      const html = await (await get('/fr/pricing')).text();
      expect(html).toMatch(/<a href="\/fr\/pricing"[^>]*>pricing<\/a>/);
      expect(html).toMatch(/<a href="\/docs\/intro"[^>]*>docs<\/a>/);
      expect(html).toMatch(/<a href="\/docs\/intro#a"[^>]*>docs-object<\/a>/);
      expect(await (await get('/pricing')).text()).toMatch(/<a href="\/pricing"[^>]*>pricing<\/a>/);
    },
  );

  specTest(
    'links a path that names a locale as written, from any locale',
    { feature, requirement: 'localized-links', check: 'a-link-that-names-a-locale-stays-as-written' },
    async () => {
      expect(await (await get('/fr/pricing')).text()).toMatch(/<a href="\/ar\/pricing"[^>]*>arabic<\/a>/);
      expect(await (await get('/pricing')).text()).toMatch(/<a href="\/ar\/pricing"[^>]*>arabic<\/a>/);
    },
  );

  specTest(
    'links each locale with hreflang, lang and its own name',
    { feature, requirement: 'locale-switcher', check: 'the-switcher-links-each-locale-with-hreflang-lang-and-endonym' },
    async () => {
      const html = await (await get('/fr/pricing')).text();
      expect(html).toContain('<nav aria-label="Language">');
      expect(html).toMatch(/<a href="\/pricing" hrefLang="en" lang="en"[^>]*>English<\/a>/i);
      expect(html).toMatch(/<a href="\/ar\/pricing" hrefLang="ar" lang="ar"[^>]*>العربية<\/a>/i);
    },
  );

  specTest(
    'marks the current locale with aria-current',
    { feature, requirement: 'locale-switcher', check: 'the-current-locale-carries-aria-current' },
    async () => {
      const html = await (await get('/fr/pricing')).text();
      expect(html).toMatch(/<a href="\/fr\/pricing" hrefLang="fr" lang="fr" aria-current="page"[^>]*>Français<\/a>/i);
      expect(html).not.toMatch(/hrefLang="en" lang="en" aria-current/i);
    },
  );

  specTest(
    'redirects the root to the preferred locale and varies on its inputs',
    { feature, requirement: 'entry-negotiation', check: 'the-root-redirects-to-the-preferred-locale-with-vary' },
    async () => {
      const negotiated = await get('/?ref=mail', { 'accept-language': 'fr-CA,fr;q=0.9,en;q=0.5' });
      expect(negotiated.status).toBe(302);
      expect(negotiated.headers.get('location')).toBe('/fr?ref=mail');
      expect(negotiated.headers.get('vary')).toContain('Accept-Language');
      const chosen = await get('/', { 'accept-language': 'fr', cookie: 'putnami-locale=en' });
      expect(chosen.status).toBe(200);
      expect(chosen.headers.get('vary')).toContain('Cookie');
      expect((await get('/', { cookie: 'putnami-locale=ar' })).headers.get('location')).toBe('/ar');
      expect((await get('/')).status).toBe(200);
    },
  );

  specTest(
    'never redirects a content path',
    { feature, requirement: 'entry-negotiation', check: 'a-content-path-is-never-redirected' },
    async () => {
      expect((await get('/pricing', { 'accept-language': 'fr' })).status).toBe(200);
      expect((await get('/fr', { 'accept-language': 'en' })).status).toBe(200);
    },
  );
});

describe('localized static render', () => {
  const Greeting = island().render(function Greeting() {
    return React.createElement('span', null, useT(footer).note());
  });

  function StaticPage() {
    return React.createElement(
      'main',
      null,
      React.createElement('h1', null, useT(home).title()),
      React.createElement(Greeting),
    );
  }

  const routes = (): RouteObject[] => [
    { path: '/', children: [{ path: 'pricing', element: React.createElement(StaticPage) }] },
  ];

  specTest(
    'ships only the namespaces the static page rendered',
    { feature, requirement: 'message-payload', check: 'a-static-page-ships-only-its-rendered-namespaces' },
    async () => {
      const result = await renderStaticDocument({
        routes: routes(),
        route: '/pricing',
        pathname: '/fr/pricing',
        i18n,
        islandsScript: '/islands.js',
      });
      expect(result.html).toContain('<html lang="fr" dir="ltr">');
      expect(result.html).toContain('<h1>Tarifs</h1>');
      expect(result.html).toContain('Fait à Paris');
      expect(readPayload(result.html)?.messages).toEqual({
        footer: { note: 'Fait à Paris' },
        home: { title: 'Tarifs' },
      });
      expect(result.html).toContain('hreflang="x-default"');
    },
  );

  it('ships no payload on a static page without islands', async () => {
    const result = await renderStaticDocument({
      routes: [{ path: '/', children: [{ path: 'docs', element: React.createElement(Docs) }] }],
      route: '/docs',
      pathname: '/fr/docs',
      i18n,
    });
    expect(readPayload(result.html)).toBeUndefined();
  });
});

describe('locale head', () => {
  specTest(
    'writes no canonical or alternate links on a noindex page',
    { feature, requirement: 'alternate-links', check: 'a-noindex-page-has-no-locale-links' },
    () => {
      const settings = parseLocaleSettings({
        locales: ['en', 'fr'],
        defaultLocale: 'en',
        siteUrl: 'https://example.com',
      });
      const meta = { metas: [{ name: 'robots', content: 'noindex' }] };
      applyLocaleHead(meta, { settings, locale: 'fr', path: '/nothing', messages: {} });
      expect(meta).toEqual({ links: [], metas: [{ name: 'robots', content: 'noindex' }] });
    },
  );
});

describe('localized manifest rows', () => {
  specTest(
    'lists each localized route',
    { feature, requirement: 'locale-routes', check: 'the-manifest-lists-each-localized-route' },
    () => {
      const rows = localizeRows(settings, [
        { route: '/', mode: 'ssr' },
        { route: '/blog/*', expandedPaths: ['/blog/a', '/fr/blog/a'] },
        { route: '/docs/intro', mode: 'ssr' },
      ]);
      expect(rows.map((r) => r.route)).toEqual([
        '/',
        '/fr',
        '/ar',
        '/blog/*',
        '/fr/blog/*',
        '/ar/blog/*',
        '/docs/intro',
      ]);
      expect(rows[3]?.expandedPaths).toEqual(['/blog/a']);
      expect(rows[4]?.expandedPaths).toEqual(['/fr/blog/a']);
      expect(rows[5]?.expandedPaths).toEqual([]);
    },
  );

  it('lists the redirect of each excluded route under each other locale', () => {
    const rows = excludedRedirectRows(settings, [
      { route: '/pricing', evidencePath: 'src/app/pricing/page.tsx', hasAction: true },
      { route: '/docs', evidencePath: 'src/app/docs/page.tsx', hasAction: false },
      {
        route: '/docs/*',
        evidencePath: 'src/app/docs/[...slug]/page.tsx',
        hasAction: true,
        expandedPaths: ['/docs/a'],
      },
    ]);
    expect(rows).toEqual([
      { route: '/fr/docs', evidencePath: 'src/app/docs/page.tsx', hasAction: false },
      { route: '/ar/docs', evidencePath: 'src/app/docs/page.tsx', hasAction: false },
      {
        route: '/fr/docs/*',
        evidencePath: 'src/app/docs/[...slug]/page.tsx',
        hasAction: false,
        expandedPaths: ['/fr/docs/a'],
      },
      {
        route: '/ar/docs/*',
        evidencePath: 'src/app/docs/[...slug]/page.tsx',
        hasAction: false,
        expandedPaths: ['/ar/docs/a'],
      },
    ]);
    expect(excludedRedirectRows(undefined, [{ route: '/docs/*', evidencePath: 'x', hasAction: false }])).toEqual([]);
  });

  it('refuses i18n on an application mounted below the site root', () => {
    const app = new ReactApplication(createMockReactConfig({}), new HttpPlugin()).i18n(i18n);
    expect(() => app.setBasename('/app')).toThrow('mounted at the site root');
    expect(() => app.i18n(i18n)).not.toThrow();
    app.reactPage('/', { page: { default: () => null } });
    expect(() => app.i18n(i18n)).toThrow('before any route');
  });
});
