import type { DocumentMeta } from '../../client/document/document.types';
import { escapeHtml } from '../../client/document/tag.utils';
import {
  isLocalizedPath,
  type LocaleServing,
  type LocaleSettings,
  localePrefix,
  localizedUrl,
  openGraphLocale,
  splitLocalePath,
  textDirection,
} from '../../shared/i18n/locale';
import type { I18nState, LocaleMessages } from '../../shared/i18n/messages';
import { I18N_PAYLOAD_ID, type I18nPayload } from '../../shared/i18n/payload';
import { safeJsonForScript } from '../safe-json';

/** What the generated application module hands the runtime. */
export interface I18nRuntimeOptions {
  settings: LocaleSettings;
  /** Translations by non-default locale; the default locale reads the code. */
  messages: Readonly<Record<string, LocaleMessages>>;
}

/**
 * The locale state of one render, from the routed path without the
 * application's own module prefix: `/fr/pricing` for the French page, on its
 * domain too. `collect` records the namespaces the render reads, which a
 * static page's payload is cut to. `served` says where the page is served;
 * only a locale with a domain is served at one.
 */
export function createI18nState(
  options: I18nRuntimeOptions,
  pathname: string,
  collect: boolean,
  served: LocaleServing = 'prefix',
): I18nState {
  const { locale, path } = splitLocalePath(options.settings, pathname);
  return {
    settings: options.settings,
    locale,
    path,
    served: served === 'domain' && options.settings.domains[locale] === undefined ? 'site' : served,
    messages: options.messages[locale] ?? {},
    ...(collect ? { rendered: new Set<string>() } : {}),
  };
}

/**
 * The router basename of a render: the module prefix, then the locale prefix,
 * except on the locale's domain, which serves it at its root.
 */
export function localeBasename(state: I18nState, moduleBase: string | undefined): string | undefined {
  const prefix = state.served === 'domain' ? '' : localePrefix(state.settings, state.locale);
  if (!prefix) return moduleBase;
  return `${moduleBase ?? ''}${prefix}`;
}

/** The pathname a request names below the application's module prefix. */
export function pathBelowBase(pathname: string, moduleBase: string | undefined): string {
  if (!moduleBase || moduleBase === '/') return pathname;
  if (pathname === moduleBase) return '/';
  return pathname.startsWith(`${moduleBase}/`) ? pathname.slice(moduleBase.length) : pathname;
}

/** Set the page's language before rendering, so a page's own `<Lang>` still wins. */
export function seedDocumentLocale(documentMeta: DocumentMeta, state: I18nState): void {
  documentMeta.lang ??= state.locale;
}

/**
 * Add the language metadata of a localized page: a canonical link to itself
 * unless the page set one, an alternate per locale plus `x-default`, and
 * `og:locale` with its alternates. A route served in the default locale only
 * gets its canonical link and nothing else; a `noindex` page gets none of them.
 */
export function applyLocaleHead(documentMeta: DocumentMeta, state: I18nState): void {
  const { settings, locale, path } = state;
  documentMeta.links ??= [];
  documentMeta.metas ??= [];
  const { links, metas } = documentMeta;
  // A page kept out of the index (a not-found page) has no URL to point to.
  if (metas.some((m) => m.name?.toLowerCase() === 'robots' && /noindex/i.test(String(m.content ?? '')))) return;
  if (!links.some((l) => l.rel === 'canonical')) {
    links.push({ rel: 'canonical', href: localizedUrl(settings, path, locale) });
  }
  if (!isLocalizedPath(settings, path) || settings.locales.length < 2) return;
  for (const alternate of settings.locales) {
    links.push({ rel: 'alternate', hrefLang: alternate, href: localizedUrl(settings, path, alternate) });
  }
  links.push({ rel: 'alternate', hrefLang: 'x-default', href: localizedUrl(settings, path, settings.defaultLocale) });
  if (!metas.some((m) => m.property === 'og:locale')) {
    metas.push({ property: 'og:locale', content: openGraphLocale(locale) });
  }
  for (const alternate of settings.locales) {
    if (alternate !== locale) metas.push({ property: 'og:locale:alternate', content: openGraphLocale(alternate) });
  }
}

/** The `lang` and `dir` attributes of the `<html>` element. */
export function htmlAttributes(lang: string, state: I18nState | undefined): string {
  const dir = state ? ` dir="${textDirection(lang)}"` : '';
  return `lang="${escapeHtml(lang)}"${dir}`;
}

/**
 * The suggestion namespace in every non-default locale: `LocaleSuggestion`
 * speaks the locale it suggests, not the page's. The default locale's text is
 * in the code.
 */
function suggestionsPayload(options: I18nRuntimeOptions): Pick<I18nPayload, 'suggestions'> {
  const namespace = options.settings.suggestion;
  if (!namespace) return {};
  const suggestions: Record<string, Readonly<Record<string, string>>> = {};
  for (const locale of options.settings.locales) {
    const entries = options.messages[locale]?.[namespace];
    if (entries) suggestions[locale] = entries;
  }
  return { suggestions };
}

/**
 * The locale payload a browser tree reads before it hydrates. `whole` ships the
 * locale's full catalog, because a hydrated page can navigate to any
 * namespace; otherwise only the namespaces the render read. The default locale
 * ships no catalog: its text is in the code.
 */
export function i18nPayloadScript(options: I18nRuntimeOptions, state: I18nState, whole: boolean): string {
  const catalog = options.messages[state.locale] ?? {};
  const messages: Record<string, Readonly<Record<string, string>>> = {};
  if (state.locale !== state.settings.defaultLocale) {
    const namespaces = whole ? Object.keys(catalog) : [...(state.rendered ?? [])];
    for (const namespace of namespaces.sort()) {
      const entries = catalog[namespace];
      if (entries) messages[namespace] = entries;
    }
  }
  const payload: I18nPayload = {
    settings: state.settings,
    locale: state.locale,
    path: state.path,
    served: state.served,
    messages,
    ...suggestionsPayload(options),
  };
  return `<script type="application/json" id="${I18N_PAYLOAD_ID}">${safeJsonForScript(payload)}</script>`;
}
