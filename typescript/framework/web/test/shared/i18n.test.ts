import { describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { formatIcu, icuArguments, IcuSyntaxError, parseIcu } from '../../src/shared/i18n/icu';
import {
  isLocalizedPath,
  LocaleSettingsError,
  localeEndonym,
  localizePath,
  localizedUrl,
  negotiateLocale,
  parseLocaleSettings,
  preferredLocale,
  splitLocalePath,
  textDirection,
} from '../../src/shared/i18n/locale';
import { messages, translator } from '../../src/shared/i18n/messages';

const feature = 'typescript/web-localization';

const settings = parseLocaleSettings({
  locales: ['en', 'fr', 'pt-BR'],
  defaultLocale: 'en',
  siteUrl: 'https://example.com/',
  exclude: ['/docs/**'],
});

const format = (message: string, locale: string, values?: Record<string, unknown>) =>
  formatIcu(parseIcu(message), locale, values as never);

describe('parseLocaleSettings', () => {
  specTest(
    'refuses a malformed block before anything else runs',
    { feature, requirement: 'locale-configuration', check: 'an-invalid-i18n-block-fails-before-generation-writes' },
    () => {
      expect(() => parseLocaleSettings('fr')).toThrow(LocaleSettingsError);
      expect(() => parseLocaleSettings({ locales: [], defaultLocale: 'en', siteUrl: 'https://x.dev' })).toThrow(
        'at least one locale',
      );
      expect(() =>
        parseLocaleSettings({ locales: ['en', 'not a tag'], defaultLocale: 'en', siteUrl: 'https://x.dev' }),
      ).toThrow('well-formed');
      expect(() =>
        parseLocaleSettings({ locales: ['en', 'EN'], defaultLocale: 'en', siteUrl: 'https://x.dev' }),
      ).toThrow('twice');
      expect(() => parseLocaleSettings({ locales: ['en'], defaultLocale: 'en', siteUrl: '/relative' })).toThrow(
        'absolute URL',
      );
      expect(() =>
        parseLocaleSettings({ locales: ['en'], defaultLocale: 'en', siteUrl: 'https://x.dev', extra: 1 }),
      ).toThrow('unknown key');
      expect(() =>
        parseLocaleSettings({ locales: ['en'], defaultLocale: 'en', siteUrl: 'https://x.dev', exclude: ['docs'] }),
      ).toThrow('start with /');
    },
  );

  specTest(
    'requires the default locale among the locales',
    { feature, requirement: 'locale-configuration', check: 'the-default-locale-must-be-listed' },
    () => {
      expect(() => parseLocaleSettings({ locales: ['fr'], defaultLocale: 'en', siteUrl: 'https://x.dev' })).toThrow(
        "'en' is not listed",
      );
    },
  );

  it('puts the default locale first, canonicalizes tags and trims the site URL', () => {
    const parsed = parseLocaleSettings({
      locales: ['fr', 'en', 'pt-br'],
      defaultLocale: 'en',
      siteUrl: 'https://x.dev/',
    });
    expect(parsed.locales).toEqual(['en', 'fr', 'pt-BR']);
    expect(parsed.siteUrl).toBe('https://x.dev');
  });
});

describe('locale paths', () => {
  it('reads the locale from the first segment only for non-default locales', () => {
    expect(splitLocalePath(settings, '/fr/pricing')).toEqual({ locale: 'fr', path: '/pricing' });
    expect(splitLocalePath(settings, '/fr')).toEqual({ locale: 'fr', path: '/' });
    expect(splitLocalePath(settings, '/pt-BR/a/')).toEqual({ locale: 'pt-BR', path: '/a' });
    // The prefix matches the tag exactly, as the registered routes do.
    expect(splitLocalePath(settings, '/pt-br/a')).toEqual({ locale: 'en', path: '/pt-br/a' });
    expect(splitLocalePath(settings, '/FR/pricing')).toEqual({ locale: 'en', path: '/FR/pricing' });
    expect(splitLocalePath(settings, '/en/pricing')).toEqual({ locale: 'en', path: '/en/pricing' });
    expect(splitLocalePath(settings, '/france')).toEqual({ locale: 'en', path: '/france' });
  });

  it('localizes paths and leaves excluded routes in the default locale', () => {
    expect(localizePath(settings, '/', 'fr')).toBe('/fr');
    expect(localizePath(settings, '/pricing', 'fr')).toBe('/fr/pricing');
    expect(localizePath(settings, '/pricing', 'en')).toBe('/pricing');
    expect(localizePath(settings, '/docs/intro', 'fr')).toBe('/docs/intro');
    expect(isLocalizedPath(settings, '/docs')).toBe(false);
    expect(isLocalizedPath(settings, '/docs/:slug')).toBe(false);
    expect(isLocalizedPath(settings, '/documentation')).toBe(true);
    expect(localizedUrl(settings, '/', 'en')).toBe('https://example.com/');
    expect(localizedUrl(settings, '/a', 'fr')).toBe('https://example.com/fr/a');
  });

  it('names locales in their own language and knows their direction', () => {
    expect(localeEndonym('fr')).toBe('Français');
    expect(localeEndonym('de')).toBe('Deutsch');
    expect(textDirection('ar-EG')).toBe('rtl');
    expect(textDirection('fr')).toBe('ltr');
  });

  it('negotiates Accept-Language by weight, then by language', () => {
    expect(negotiateLocale(settings, 'fr-CA,fr;q=0.9,en;q=0.8')).toBe('fr');
    expect(negotiateLocale(settings, 'de, pt;q=0.5')).toBe('pt-BR');
    expect(negotiateLocale(settings, 'en;q=0.4, fr;q=0.9')).toBe('fr');
    expect(negotiateLocale(settings, 'de, *')).toBeUndefined();
    expect(negotiateLocale(settings, null)).toBeUndefined();
  });

  it('reads the preference cookie only when it names a published locale', () => {
    expect(preferredLocale(settings, 'a=1; putnami-locale=fr')).toBe('fr');
    expect(preferredLocale(settings, 'putnami-locale=de')).toBeUndefined();
    expect(preferredLocale(settings, 'putnami-locale=%E0%A4%A')).toBeUndefined();
  });
});

describe('ICU messages', () => {
  specTest(
    'formats plural, ordinal, select and number arguments through Intl',
    { feature, requirement: 'typed-messages', check: 'plural-select-and-number-arguments-format-through-intl' },
    () => {
      const seats = '{count, plural, =0 {no seat} one {# seat} other {# seats}}';
      expect(format(seats, 'en', { count: 0 })).toBe('no seat');
      expect(format(seats, 'en', { count: 1 })).toBe('1 seat');
      expect(format(seats, 'en', { count: 1200 })).toBe('1,200 seats');
      expect(format('{count, plural, one {# siège} other {# sièges}}', 'fr', { count: 1200 })).toBe('1 200 sièges');
      expect(format('{n, selectordinal, one {#st} two {#nd} few {#rd} other {#th}}', 'en', { n: 22 })).toBe('22nd');
      expect(format('{who, select, team {the team} other {{who}}}', 'en', { who: 'Ada' })).toBe('Ada');
      expect(format('{who, select, team {the team} other {{who}}}', 'en', { who: 'team' })).toBe('the team');
      expect(format('{ratio, number, percent}', 'fr', { ratio: 0.5 })).toBe('50 %');
      expect(format('{when, date, long}', 'en', { when: new Date(Date.UTC(2026, 10, 1, 12)) })).toContain('2026');
      expect(format("It''s '{literal}'", 'en')).toBe("It's {literal}");
    },
  );

  it('refuses what the formatter would print wrongly', () => {
    expect(() => parseIcu('{name')).toThrow(IcuSyntaxError);
    expect(() => parseIcu('{n, plural, one {x}}')).toThrow("'other'");
    expect(() => parseIcu('{n, spellout}')).toThrow('Unsupported argument type');
    expect(() => parseIcu('{n, number, ::currency/EUR}')).toThrow(IcuSyntaxError);
    expect(() => parseIcu('a } b')).toThrow(IcuSyntaxError);
  });

  it('lists the arguments a message reads, in nested branches too', () => {
    const args = icuArguments(parseIcu('{n, plural, one {{name} has # item} other {{name} has # items}} {d, date}'));
    expect([...args.entries()]).toEqual([
      ['n', 'plural'],
      ['name', 'string'],
      ['d', 'date'],
    ]);
  });
});

describe('messages and translator', () => {
  const home = messages('home', {
    title: 'Pricing',
    seats: { text: '{count, plural, one {# seat} other {# seats}}', context: 'Price card' },
  });

  specTest(
    'formats from the locale catalog',
    { feature, requirement: 'typed-messages', check: 'a-message-formats-from-the-locale-catalog' },
    () => {
      const t = translator(home, 'fr', {
        home: { title: 'Tarifs', seats: '{count, plural, one {# siège} other {# sièges}}' },
      });
      expect(t.title()).toBe('Tarifs');
      expect(t.seats({ count: 2 })).toBe('2 sièges');
    },
  );

  specTest(
    'falls back to the default text when the catalog holds no translation',
    { feature, requirement: 'typed-messages', check: 'a-missing-translation-falls-back-to-the-default-text' },
    () => {
      const t = translator(home, 'fr', { home: { title: 'Tarifs' } });
      expect(t.seats({ count: 2 })).toBe('2 seats');
      expect(translator(home, 'en').title()).toBe('Pricing');
    },
  );

  it('refuses namespaces and keys a catalog could not hold', () => {
    expect(() => messages('Home Page', {})).toThrow('namespace');
    expect(() => messages('home', { 'bad-key': 'x' })).toThrow('identifier');
  });

  it('types the values a message needs', () => {
    const t = translator(home, 'en');
    // @ts-expect-error count is required
    expect(() => t.seats({})).not.toThrow();
    // @ts-expect-error title takes no values
    t.title({ count: 1 });
    const typed = translator(
      messages('typed', {
        greet: 'Hello {name}, you have {n, plural, one {# message from {sender}} other {# messages}}',
        when: 'Updated {at, date, short}',
      }),
      'en',
    );
    expect(typed.greet({ name: 'Ada', n: 1, sender: 'Bob' })).toBe('Hello Ada, you have 1 message from Bob');
    // @ts-expect-error sender is required by a branch
    typed.greet({ name: 'Ada', n: 1 });
    expect(typed.when({ at: new Date(Date.UTC(2026, 0, 2)) })).toContain('26');
  });

  it('reads a quoted brace as text in the types and at runtime', () => {
    const quoted = translator(messages('quoted', { use: "Write '{literal}' for {name}, it''s text" }), 'en');
    expect(quoted.use({ name: 'Ada' })).toBe("Write {literal} for Ada, it's text");
    // @ts-expect-error literal is text, not an argument
    quoted.use({ name: 'Ada', literal: 'x' });
  });
});
