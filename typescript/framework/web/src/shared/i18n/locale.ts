/**
 * Locale settings and the pure functions that map a URL to its locale.
 *
 * The locale of a page is a function of its path and these settings only: the
 * default locale is unprefixed, every other locale is the first path segment.
 * Server and browser compute it the same way, so nothing about it is stored.
 */

export interface LocaleSettings {
  /** Published locales, default first after normalization. */
  readonly locales: readonly string[];
  readonly defaultLocale: string;
  /** Absolute origin (and optional base path) alternate links are built from. */
  readonly siteUrl: string;
  /** Route patterns served in the default locale only (`/docs/**`). */
  readonly exclude: readonly string[];
}

/** The cookie an explicit choice in the locale switcher writes. */
export const LOCALE_COOKIE = 'putnami-locale';

const RTL_LANGUAGES = new Set(['ar', 'arc', 'ckb', 'dv', 'fa', 'he', 'ks', 'ku', 'ps', 'sd', 'ug', 'ur', 'yi']);

export class LocaleSettingsError extends Error {
  constructor(message: string) {
    super(`Invalid @putnami/web i18n configuration: ${message}`);
    this.name = 'LocaleSettingsError';
  }
}

function canonicalLocale(tag: unknown, where: string): string {
  if (typeof tag !== 'string' || tag.trim() === '') {
    throw new LocaleSettingsError(`${where} must be a non-empty BCP 47 language tag`);
  }
  try {
    const [canonical] = Intl.getCanonicalLocales(tag);
    if (!canonical) throw new Error('empty');
    return canonical;
  } catch {
    throw new LocaleSettingsError(`${where} '${tag}' is not a well-formed BCP 47 language tag`);
  }
}

/**
 * Validate the `i18n` block of the generation configuration. Throws a
 * {@link LocaleSettingsError} naming the first defect; never returns a partial
 * value.
 */
export function parseLocaleSettings(raw: unknown): LocaleSettings {
  if (raw === null || typeof raw !== 'object' || Array.isArray(raw)) {
    throw new LocaleSettingsError('i18n must be an object');
  }
  const block = raw as Record<string, unknown>;
  const known = new Set(['locales', 'defaultLocale', 'siteUrl', 'exclude']);
  for (const key of Object.keys(block)) {
    if (!known.has(key)) throw new LocaleSettingsError(`unknown key i18n.${key}`);
  }
  if (!Array.isArray(block['locales']) || block['locales'].length === 0) {
    throw new LocaleSettingsError('i18n.locales must list at least one locale');
  }
  const locales = block['locales'].map((tag, i) => canonicalLocale(tag, `i18n.locales[${i}]`));
  const lowered = locales.map((l) => l.toLowerCase());
  const duplicate = lowered.find((l, i) => lowered.indexOf(l) !== i);
  if (duplicate) throw new LocaleSettingsError(`i18n.locales lists '${duplicate}' twice`);

  const defaultLocale = canonicalLocale(block['defaultLocale'], 'i18n.defaultLocale');
  if (!locales.includes(defaultLocale)) {
    throw new LocaleSettingsError(`i18n.defaultLocale '${defaultLocale}' is not listed in i18n.locales`);
  }

  const siteUrlRaw = block['siteUrl'];
  if (typeof siteUrlRaw !== 'string') {
    throw new LocaleSettingsError('i18n.siteUrl must be the absolute URL of the site, such as https://example.com');
  }
  let siteUrl: URL;
  try {
    siteUrl = new URL(siteUrlRaw);
  } catch {
    throw new LocaleSettingsError(`i18n.siteUrl '${siteUrlRaw}' is not an absolute URL`);
  }
  if (siteUrl.protocol !== 'https:' && siteUrl.protocol !== 'http:') {
    throw new LocaleSettingsError(`i18n.siteUrl '${siteUrlRaw}' must use http or https`);
  }
  if (siteUrl.search || siteUrl.hash) {
    throw new LocaleSettingsError(`i18n.siteUrl '${siteUrlRaw}' must not carry a query or a fragment`);
  }

  const excludeRaw = block['exclude'] ?? [];
  if (!Array.isArray(excludeRaw) || excludeRaw.some((p) => typeof p !== 'string' || !p.startsWith('/'))) {
    throw new LocaleSettingsError('i18n.exclude must list route patterns that start with /');
  }

  return {
    locales: [defaultLocale, ...locales.filter((l) => l !== defaultLocale)],
    defaultLocale,
    siteUrl: siteUrl.toString().replace(/\/$/, ''),
    exclude: [...(excludeRaw as string[])].sort(),
  };
}

/** `''` for the default locale, `/<locale>` otherwise. */
export function localePrefix(settings: LocaleSettings, locale: string): string {
  return locale === settings.defaultLocale ? '' : `/${locale}`;
}

/** A path with a leading slash and no trailing one: `/docs/why/` reads `/docs/why`. */
export function normalizePath(path: string): string {
  if (!path.startsWith('/')) path = `/${path}`;
  return path.length > 1 && path.endsWith('/') ? path.slice(0, -1) : path;
}

export interface LocalizedPath {
  locale: string;
  /** The path with its locale prefix removed; `/` for a locale root. */
  path: string;
}

/**
 * Read the locale a path names, and the path without its prefix. The prefix
 * matches the locale tag exactly, as HTTP routes do: `/FR/pricing` names none.
 */
export function splitLocalePath(settings: LocaleSettings, pathname: string): LocalizedPath {
  const path = normalizePath(pathname);
  const segment = path.split('/')[1] ?? '';
  const locale = settings.locales.find((l) => l !== settings.defaultLocale && l === segment);
  if (!locale) return { locale: settings.defaultLocale, path };
  const rest = path.slice(segment.length + 1);
  return { locale, path: rest === '' ? '/' : rest };
}

function patternToRegExp(pattern: string): RegExp {
  const normalized = normalizePath(pattern);
  let source = '';
  for (let i = 0; i < normalized.length; i++) {
    const ch = normalized[i] as string;
    if (ch === '*' && normalized[i + 1] === '*') {
      // `/docs/**` covers `/docs` itself and everything below it.
      if (source.endsWith('/')) source = `${source.slice(0, -1)}(?:/.*)?`;
      else source += '.*';
      i++;
    } else if (ch === '*') {
      source += '[^/]*';
    } else {
      source += ch.replace(/[.+?^${}()|[\]\\]/g, '\\$&');
    }
  }
  return new RegExp(`^${source}$`);
}

const patternCache = new Map<string, RegExp>();

/**
 * Whether a locale-free path or route pattern exists in every locale. Route
 * patterns are matched with their parameters as written (`/docs/:slug`).
 */
export function isLocalizedPath(settings: LocaleSettings, path: string): boolean {
  const target = normalizePath(path);
  return !settings.exclude.some((pattern) => {
    let re = patternCache.get(pattern);
    if (!re) {
      re = patternToRegExp(pattern);
      patternCache.set(pattern, re);
    }
    return re.test(target);
  });
}

/** The path of a locale-free path in a given locale. */
export function localizePath(settings: LocaleSettings, path: string, locale: string): string {
  const target = normalizePath(path);
  if (!isLocalizedPath(settings, target)) return target;
  const prefix = localePrefix(settings, locale);
  if (!prefix) return target;
  return target === '/' ? prefix : `${prefix}${target}`;
}

/** The absolute URL of a locale-free path in a given locale. */
export function localizedUrl(settings: LocaleSettings, path: string, locale: string): string {
  const localized = localizePath(settings, path, locale);
  return `${settings.siteUrl}${localized === '/' ? '/' : localized}`;
}

export function textDirection(locale: string): 'ltr' | 'rtl' {
  const language = locale.split('-')[0]?.toLowerCase() ?? '';
  return RTL_LANGUAGES.has(language) ? 'rtl' : 'ltr';
}

/** The locale's own name, written in that locale: `Français`, `Deutsch`. */
export function localeEndonym(locale: string): string {
  try {
    const name = new Intl.DisplayNames([locale], { type: 'language' }).of(locale);
    if (name) return name.charAt(0).toLocaleUpperCase(locale) + name.slice(1);
  } catch {
    // An engine without DisplayNames data falls back to the tag.
  }
  return locale;
}

/** `og:locale` spells a locale with an underscore: `fr_CA`. */
export function openGraphLocale(locale: string): string {
  return locale.replace('-', '_');
}

/**
 * Pick the best published locale for an `Accept-Language` header: an exact tag
 * first, then the same language. Returns `undefined` when nothing matches.
 */
export function negotiateLocale(settings: LocaleSettings, acceptLanguage: string | null): string | undefined {
  if (!acceptLanguage) return undefined;
  const ranges = acceptLanguage
    .split(',')
    .map((part, index) => {
      const [tag = '', ...params] = part.trim().split(';');
      const q = params.map((p) => p.trim()).find((p) => p.startsWith('q='));
      const weight = q ? Number(q.slice(2)) : 1;
      return { tag: tag.trim().toLowerCase(), weight: Number.isFinite(weight) ? weight : 0, index };
    })
    .filter((r) => r.tag && r.tag !== '*' && r.weight > 0)
    .sort((a, b) => b.weight - a.weight || a.index - b.index);
  for (const range of ranges) {
    const exact = settings.locales.find((l) => l.toLowerCase() === range.tag);
    if (exact) return exact;
    const language = range.tag.split('-')[0];
    const sameLanguage = settings.locales.find((l) => l.toLowerCase().split('-')[0] === language);
    if (sameLanguage) return sameLanguage;
  }
  return undefined;
}

/** Read the locale preference cookie, when it names a published locale. */
export function preferredLocale(settings: LocaleSettings, cookieHeader: string | null): string | undefined {
  if (!cookieHeader) return undefined;
  for (const part of cookieHeader.split(';')) {
    const [name, ...value] = part.trim().split('=');
    if (name === LOCALE_COOKIE) {
      let tag: string;
      try {
        tag = decodeURIComponent(value.join('=')).toLowerCase();
      } catch {
        return undefined;
      }
      return settings.locales.find((l) => l.toLowerCase() === tag);
    }
  }
  return undefined;
}
