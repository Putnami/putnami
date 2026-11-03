import { afterEach, describe, expect, it, mock } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import React, { type ReactElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { LocaleSuggestion, SuggestionLine } from '../../src/client/i18n/locale-suggestion';
import type { LocaleInfo } from '../../src/client/i18n/locale-switcher';
import { localeHref, parseLocaleSettings, suggestedLocale } from '../../src/shared/i18n/locale';
import { I18nProvider, type I18nState, messages } from '../../src/shared/i18n/messages';
import { I18N_PAYLOAD_ID, readI18nPayload } from '../../src/shared/i18n/payload';
import { createI18nState, i18nPayloadScript } from '../../src/ssr/i18n/runtime';

const feature = 'typescript/web-localization';
const requirement = 'locale-suggestion';

const settings = parseLocaleSettings({
  locales: ['en', 'fr'],
  defaultLocale: 'en',
  siteUrl: 'https://example.com',
  exclude: ['/docs/**'],
  paths: { fr: { '/pricing': '/tarifs' } },
  suggestion: 'locale-suggestion',
});

const suggestion = messages('locale-suggestion', {
  message: 'Read this page in <link>English</link>.',
  dismiss: 'Dismiss',
});
const catalogs = {
  fr: { 'locale-suggestion': { message: 'Lire cette page en <link>français</link>.', dismiss: 'Fermer' } },
};

function stateOf(locale: string, path: string): I18nState {
  const options = { settings, messages: catalogs };
  const html = i18nPayloadScript(options, createI18nState(options, locale === 'en' ? path : `/fr${path}`, false), true);
  const text = html.slice(html.indexOf('>') + 1, html.lastIndexOf('</script>'));
  const state = readI18nPayload({
    getElementById: (id: string) => (id === I18N_PAYLOAD_ID ? { textContent: text } : null),
  } as unknown as Document);
  if (!state) throw new Error('no payload');
  return state;
}

function infoOf(state: I18nState): LocaleInfo {
  return {
    locale: state.locale,
    defaultLocale: settings.defaultLocale,
    locales: settings.locales,
    dir: 'ltr',
    path: state.path,
    localized: true,
    localize: (path, locale = state.locale) => localeHref(settings, path, locale, state),
  };
}

function line(state: I18nState, suggested: string, onDismiss = () => {}): ReactElement {
  return SuggestionLine({ state, info: infoOf(state), suggested, messages: suggestion, onDismiss }) as ReactElement;
}

describe('LocaleSuggestion', () => {
  const realDocument = globalThis.document;
  afterEach(() => {
    globalThis.document = realDocument;
  });

  specTest(
    'offers the page in the locale the reader prefers, in that locale',
    { feature, requirement, check: 'the-suggestion-offers-the-page-in-the-preferred-locale' },
    () => {
      const english = { locale: 'en', path: '/pricing' };
      expect(suggestedLocale(settings, english, null, ['fr-FR', 'en'])).toBe('fr');
      expect(suggestedLocale(settings, english, null, ['en-GB', 'fr'])).toBeUndefined();
      expect(suggestedLocale(settings, english, null, ['de'])).toBeUndefined();
      // The reader's choice wins over the browser languages.
      expect(suggestedLocale(settings, english, 'putnami-locale=en', ['fr'])).toBeUndefined();
      expect(suggestedLocale(settings, { locale: 'fr', path: '/pricing' }, 'putnami-locale=en', ['fr'])).toBe('en');
      // A page served in the default locale only has nothing to offer.
      expect(suggestedLocale(settings, { locale: 'en', path: '/docs/intro' }, null, ['fr'])).toBeUndefined();

      expect(renderToStaticMarkup(line(stateOf('en', '/pricing'), 'fr'))).toBe(
        '<aside lang="fr" dir="ltr"><p>Lire cette page en <a href="/fr/tarifs?locale=fr" hrefLang="fr">français</a>.</p><button type="button">Fermer</button></aside>',
      );
      expect(renderToStaticMarkup(line(stateOf('fr', '/pricing'), 'en'))).toContain(
        '<p>Read this page in <a href="/pricing?locale=en" hrefLang="en">English</a>.</p>',
      );
      // The server, and the browser before it decides, render nothing.
      const page = React.createElement(
        I18nProvider,
        { value: stateOf('en', '/pricing') },
        React.createElement(LocaleSuggestion, { messages: suggestion }),
      );
      expect(renderToStaticMarkup(page)).toBe('');
    },
  );

  specTest(
    "records the page's locale when the reader dismisses the suggestion",
    { feature, requirement, check: 'dismissing-the-suggestion-records-the-page-locale' },
    () => {
      const jar = { cookie: '' };
      globalThis.document = jar as unknown as Document;
      const onDismiss = mock(() => {});
      const { children } = line(stateOf('fr', '/pricing'), 'en', onDismiss).props as { children: ReactElement[] };
      const button = children[1];
      if (!button) throw new Error('the suggestion has no dismiss button');
      (button.props as { onClick: () => void }).onClick();
      expect(jar.cookie).toStartWith('putnami-locale=fr;');
      expect(onDismiss).toHaveBeenCalledTimes(1);
    },
  );

  specTest(
    'sends the suggestion namespace in every locale, on every page',
    { feature, requirement, check: 'the-payload-carries-the-suggestion-namespace-in-every-locale' },
    () => {
      const french = { fr: catalogs.fr['locale-suggestion'] };
      expect(stateOf('en', '/pricing').suggestions as unknown).toEqual(french);
      expect(stateOf('fr', '/docs/intro').suggestions as unknown).toEqual(french);
      const without = parseLocaleSettings({ ...settings, suggestion: undefined });
      const html = i18nPayloadScript(
        { settings: without, messages: catalogs },
        createI18nState({ settings: without, messages: catalogs }, '/pricing', false),
        true,
      );
      expect(html).not.toContain('suggestions');
    },
  );

  it('refuses a suggestion namespace messages() would refuse', () => {
    expect(() => parseLocaleSettings({ ...settings, suggestion: 'Locale Suggestion' })).toThrow(
      'i18n.suggestion must name a messages namespace',
    );
  });
});
