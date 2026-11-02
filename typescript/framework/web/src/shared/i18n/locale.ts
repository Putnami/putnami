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
  /** Translated paths by locale, then by route: `{ fr: { '/privacy': '/confidentialite' } }`. */
  readonly paths: Readonly<Record<string, Readonly<Record<string, string>>>>;
  /** The origin a locale is served at, by non-default locale: `{ fr: 'https://example.fr' }`. */
  readonly domains: Readonly<Record<string, string>>;
}

/** The cookie that records a reader's explicit locale choice. */
export const LOCALE_COOKIE = 'putnami-locale';

/** The query parameter a locale link carries its choice in: `/?locale=en`. */
export const LOCALE_PARAM = 'locale';

/** The `Set-Cookie` and `document.cookie` value that records a locale choice for a year. */
export function localeCookie(locale: string, secure: boolean): string {
  return `${LOCALE_COOKIE}=${encodeURIComponent(locale)}; Path=/; Max-Age=31536000; SameSite=Lax${secure ? '; Secure' : ''}`;
}

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
  const known = new Set(['locales', 'defaultLocale', 'siteUrl', 'exclude', 'paths', 'domains']);
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

  const settings: LocaleSettings = {
    locales: [defaultLocale, ...locales.filter((l) => l !== defaultLocale)],
    defaultLocale,
    siteUrl: siteUrl.toString().replace(/\/$/, ''),
    exclude: [...(excludeRaw as string[])].sort(),
    paths: {},
    domains: {},
  };
  return {
    ...settings,
    paths: parseTranslatedPaths(settings, block['paths'] ?? {}),
    domains: parseDomains(settings, block['domains'] ?? {}),
  };
}

/**
 * Validate `i18n.domains`: per non-default locale, the http or https origin
 * it is served at, without a path, a query or a fragment. Each origin serves
 * one locale, and none is the site's.
 */
function parseDomains(settings: LocaleSettings, raw: unknown): LocaleSettings['domains'] {
  if (raw === null || typeof raw !== 'object' || Array.isArray(raw)) {
    throw new LocaleSettingsError('i18n.domains must map a locale to the origin it is served at');
  }
  const domains: Record<string, string> = {};
  const used = new Map<string, string>([[new URL(settings.siteUrl).host, 'i18n.siteUrl']]);
  for (const [locale, value] of Object.entries(raw as Record<string, unknown>)) {
    const where = `i18n.domains.${locale}`;
    if (!settings.locales.includes(locale) || locale === settings.defaultLocale) {
      throw new LocaleSettingsError(`${where}: '${locale}' is not a published locale other than the default`);
    }
    let url: URL | undefined;
    try {
      url = typeof value === 'string' ? new URL(value) : undefined;
    } catch {
      url = undefined;
    }
    if (
      !url ||
      (url.protocol !== 'https:' && url.protocol !== 'http:') ||
      url.pathname !== '/' ||
      url.search ||
      url.hash ||
      url.username
    ) {
      throw new LocaleSettingsError(`${where} must be an http or https origin, such as https://example.fr`);
    }
    const other = used.get(url.host);
    if (other) throw new LocaleSettingsError(`${where}: '${url.host}' is already the host of ${other}`);
    used.set(url.host, where);
    domains[locale] = url.origin;
  }
  return domains;
}

/** Whether a path matches more than one URL: `/blog/:slug`, `/docs/*`, `/blog/[slug]`. */
function hasParameters(path: string): boolean {
  return /[:*[]/.test(path);
}

/**
 * Validate `i18n.paths`: per non-default locale, a map from a localized route
 * without parameters to its path in that locale. A translated path names no
 * locale and is unique in its locale. Whether each route exists, and whether a
 * translated path collides with another route, is checked against the routes
 * at generation.
 */
function parseTranslatedPaths(settings: LocaleSettings, raw: unknown): LocaleSettings['paths'] {
  if (raw === null || typeof raw !== 'object' || Array.isArray(raw)) {
    throw new LocaleSettingsError('i18n.paths must map a locale to its translated paths');
  }
  const paths: Record<string, Record<string, string>> = {};
  for (const [locale, entries] of Object.entries(raw as Record<string, unknown>)) {
    const where = `i18n.paths.${locale}`;
    if (!settings.locales.includes(locale) || locale === settings.defaultLocale) {
      throw new LocaleSettingsError(`${where}: '${locale}' is not a published locale other than the default`);
    }
    if (entries === null || typeof entries !== 'object' || Array.isArray(entries)) {
      throw new LocaleSettingsError(`${where} must map a route to its path in ${locale}`);
    }
    const translated: Record<string, string> = {};
    const used = new Map<string, string>();
    for (const [route, path] of Object.entries(entries as Record<string, unknown>)) {
      if (typeof path !== 'string' || !route.startsWith('/') || !path.startsWith('/')) {
        throw new LocaleSettingsError(`${where} must map a route that starts with / to a path that starts with /`);
      }
      const from = normalizePath(route);
      const to = normalizePath(path);
      if (from === '/' || to === '/') throw new LocaleSettingsError(`${where}: the locale's home is not translated`);
      if (hasParameters(from) || hasParameters(to)) {
        throw new LocaleSettingsError(`${where}['${route}']: a route with parameters is not translated`);
      }
      if (settings.locales.includes(to.split('/')[1] ?? '')) {
        throw new LocaleSettingsError(`${where}['${route}']: '${to}' names a locale`);
      }
      if (!isLocalizedPath(settings, from) || !isLocalizedPath(settings, to)) {
        throw new LocaleSettingsError(`${where}['${route}']: '${from}' and '${to}' must both be localized`);
      }
      const other = used.get(to);
      if (other) throw new LocaleSettingsError(`${where}: '${other}' and '${from}' both translate to '${to}'`);
      used.set(to, from);
      if (to !== from) translated[from] = to;
    }
    if (Object.keys(translated).length > 0) paths[locale] = translated;
  }
  return paths;
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
  const rest = path.slice(segment.length + 1) || '/';
  // A translated path reads as the route it translates.
  return { locale, path: untranslatePath(settings, rest, locale) };
}

/** The route a locale's path translates: `/confidentialite` reads `/privacy` in French. */
export function untranslatePath(settings: LocaleSettings, path: string, locale: string): string {
  const target = normalizePath(path);
  const translations = settings.paths[locale] ?? {};
  return Object.keys(translations).find((r) => translations[r] === target) ?? target;
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

/** A locale-free path as a locale names it, without the locale prefix: `/privacy` reads `/confidentialite` in French. */
export function translatePath(settings: LocaleSettings, path: string, locale: string): string {
  const target = normalizePath(path);
  return settings.paths[locale]?.[target] ?? target;
}

/** The path of a locale-free path in a given locale, translated when the locale translates it. */
export function localizePath(settings: LocaleSettings, path: string, locale: string): string {
  const target = normalizePath(path);
  if (!isLocalizedPath(settings, target)) return target;
  const prefix = localePrefix(settings, locale);
  if (!prefix) return target;
  return target === '/' ? prefix : `${prefix}${translatePath(settings, target, locale)}`;
}

/** Whether a locale-free path is served in a locale at the locale's own origin. */
function onLocaleDomain(settings: LocaleSettings, path: string, locale: string): boolean {
  return settings.domains[locale] !== undefined && isLocalizedPath(settings, path);
}

/** The site URL a locale-free path is served under in a locale: its domain, else `siteUrl`. */
function siteRoot(settings: LocaleSettings, path: string, locale: string): string {
  const domain = settings.domains[locale];
  if (!domain || !onLocaleDomain(settings, path, locale)) return settings.siteUrl;
  return `${domain}${new URL(settings.siteUrl).pathname.replace(/\/$/, '')}`;
}

/**
 * The path of a locale-free path in a locale, on the origin that serves it: a
 * locale with a domain is served at the root of that domain, every other one
 * under its prefix.
 */
export function localePagePath(settings: LocaleSettings, path: string, locale: string): string {
  return onLocaleDomain(settings, path, locale)
    ? translatePath(settings, path, locale)
    : localizePath(settings, path, locale);
}

/** The absolute URL of a locale-free path in a given locale. */
export function localizedUrl(settings: LocaleSettings, path: string, locale: string): string {
  const localized = localePagePath(settings, path, locale);
  return `${siteRoot(settings, path, locale)}${localized === '/' ? '/' : localized}`;
}

/**
 * Where a page is served: `domain` at the root of its locale's domain, `site`
 * on the host of `siteUrl`, `prefix` on any other host, such as a development
 * server or a preview, which serves every locale under its prefix.
 */
export type LocaleServing = 'domain' | 'site' | 'prefix';

/** The page a link sits on. */
export interface LocaleOrigin {
  locale: string;
  served: LocaleServing;
}

/**
 * The href of a locale-free path in a locale, from a page: a path when the
 * page's origin serves it, the absolute URL when another origin does. On a
 * host that is neither the site nor a domain, every href is a path.
 */
export function localeHref(settings: LocaleSettings, path: string, locale: string, from: LocaleOrigin): string {
  const target = normalizePath(path);
  if (from.served === 'prefix') return localizePath(settings, target, locale);
  const targetDomain = onLocaleDomain(settings, target, locale) ? settings.domains[locale] : undefined;
  const fromDomain = from.served === 'domain' ? settings.domains[from.locale] : undefined;
  if (targetDomain !== fromDomain) return localizedUrl(settings, target, locale);
  return localePagePath(settings, target, locale);
}

/** The locale whose domain a host names, such as `example.fr`. */
export function domainLocale(settings: LocaleSettings, host: string): string | undefined {
  return Object.keys(settings.domains).find((locale) => new URL(settings.domains[locale] as string).host === host);
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

/** The published locale a tag names, whatever its case. */
export function publishedLocale(settings: LocaleSettings, tag: string): string | undefined {
  const lowered = tag.toLowerCase();
  return settings.locales.find((l) => l.toLowerCase() === lowered);
}

/** Read the locale preference cookie, when it names a published locale. */
export function preferredLocale(settings: LocaleSettings, cookieHeader: string | null): string | undefined {
  if (!cookieHeader) return undefined;
  for (const part of cookieHeader.split(';')) {
    const [name, ...value] = part.trim().split('=');
    if (name === LOCALE_COOKIE) {
      try {
        return publishedLocale(settings, decodeURIComponent(value.join('=')));
      } catch {
        return undefined;
      }
    }
  }
  return undefined;
}
