import { afterEach, describe, expect, it } from 'bun:test';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { Link } from '../../src/client/components/link';
import { LocaleSwitcher, rememberLocale, useLocale } from '../../src/client/i18n/locale-switcher';
import { parseLocaleSettings } from '../../src/shared/i18n/locale';
import { I18nProvider, type I18nState, messages, useT } from '../../src/shared/i18n/messages';
import { I18N_PAYLOAD_ID, readI18nPayload } from '../../src/shared/i18n/payload';

const settings = parseLocaleSettings({
  locales: ['en', 'fr'],
  defaultLocale: 'en',
  siteUrl: 'https://example.com',
  exclude: ['/docs/**'],
});

const fr: I18nState = {
  settings,
  locale: 'fr',
  path: '/pricing',
  served: 'prefix',
  messages: { home: { title: 'Tarifs' } },
};

const inFrench = (node: React.ReactNode) =>
  renderToStaticMarkup(React.createElement(I18nProvider, { value: fr }, node));

function documentWith(text: string | undefined): Document {
  return {
    getElementById: (id: string) => (id === I18N_PAYLOAD_ID && text !== undefined ? { textContent: text } : null),
  } as unknown as Document;
}

describe('readI18nPayload', () => {
  it('reads the locale state the server rendered', () => {
    const payload = {
      settings,
      locale: 'fr',
      path: '/pricing',
      served: 'prefix' as const,
      messages: { home: { title: 'Tarifs' } },
    };
    expect(readI18nPayload(documentWith(JSON.stringify(payload)))).toEqual(payload);
  });

  it('returns nothing for a page without a payload or with a broken one', () => {
    expect(readI18nPayload(documentWith(undefined))).toBeUndefined();
    expect(readI18nPayload(documentWith('{'))).toBeUndefined();
    expect(readI18nPayload(undefined)).toBeUndefined();
  });
});

describe('Link outside a router', () => {
  it('keeps the locale prefix, the query and the hash', () => {
    expect(inFrench(React.createElement(Link, { to: '/pricing?plan=team#faq' }, 'x'))).toBe(
      '<a href="/fr/pricing?plan=team#faq">x</a>',
    );
    expect(inFrench(React.createElement(Link, { to: { pathname: '/', search: '?a=1' } }, 'x'))).toBe(
      '<a href="/fr?a=1">x</a>',
    );
  });

  it('leaves excluded, external and relative targets as written', () => {
    expect(inFrench(React.createElement(Link, { to: '/docs/intro' }, 'x'))).toBe('<a href="/docs/intro">x</a>');
    expect(inFrench(React.createElement(Link, { to: 'https://github.com' }, 'x'))).toBe(
      '<a href="https://github.com">x</a>',
    );
    expect(inFrench(React.createElement(Link, { to: '//cdn.example.com/a' }, 'x'))).toBe(
      '<a href="//cdn.example.com/a">x</a>',
    );
    expect(renderToStaticMarkup(React.createElement(Link, { to: '/pricing' }, 'x'))).toBe('<a href="/pricing">x</a>');
  });
});

describe('locale hooks', () => {
  function Probe() {
    const info = useLocale();
    return React.createElement(
      'p',
      null,
      `${info.locale} ${info.dir} ${info.localize('/a')} ${info.localize('/a', 'en')}`,
    );
  }

  it('describes the current locale and localizes paths', () => {
    expect(inFrench(React.createElement(Probe))).toBe('<p>fr ltr /fr/a /a</p>');
  });

  it('falls back to the default text and locale without i18n', () => {
    const home = messages('home', { title: 'Pricing' });
    function Title() {
      return React.createElement('h1', null, useT(home).title());
    }
    expect(renderToStaticMarkup(React.createElement(Probe))).toBe('<p>en ltr /a /a</p>');
    expect(renderToStaticMarkup(React.createElement(Title))).toBe('<h1>Pricing</h1>');
    expect(inFrench(React.createElement(Title))).toBe('<h1>Tarifs</h1>');
  });

  it('renders no switcher when one locale is published', () => {
    expect(renderToStaticMarkup(React.createElement(LocaleSwitcher, { label: 'Language' }))).toBe('');
  });

  it('links an excluded page to the home of each other locale', () => {
    const docs: I18nState = { ...fr, locale: 'en', path: '/docs/intro', messages: {} };
    const html = renderToStaticMarkup(
      React.createElement(I18nProvider, { value: docs }, React.createElement(LocaleSwitcher, { label: 'Language' })),
    );
    expect(html).toContain('href="/docs/intro?locale=en" hrefLang="en"');
    expect(html).toContain('href="/fr?locale=fr" hrefLang="fr"');
  });

  it('passes the links to a custom render function', () => {
    const html = inFrench(
      <LocaleSwitcher label='Language'>
        {(links) => links.map((l) => `${l.locale}:${l.href}:${l.current}`).join(' ')}
      </LocaleSwitcher>,
    );
    expect(html).toBe('en:/pricing?locale=en:false fr:/fr/pricing?locale=fr:true');
  });
});

describe('rememberLocale', () => {
  const globals = globalThis as { document?: unknown; location?: unknown };
  const saved = { document: globals.document, location: globals.location };

  afterEach(() => {
    globals.document = saved.document;
    globals.location = saved.location;
  });

  it('writes a first-party cookie the server reads at the root', () => {
    const fake = { cookie: '' };
    globals.document = fake;
    globals.location = { protocol: 'https:' };
    rememberLocale('pt-BR');
    expect(fake.cookie).toBe('putnami-locale=pt-BR; Path=/; Max-Age=31536000; SameSite=Lax; Secure');
  });

  it('does nothing without a document', () => {
    globals.document = undefined;
    expect(() => rememberLocale('fr')).not.toThrow();
  });
});
