import { type ReactNode, useContext } from 'react';
import { useInRouterContext, useLocation } from 'react-router';
import {
  isLocalizedPath,
  LOCALE_PARAM,
  localeCookie,
  localeEndonym,
  localizePath,
  normalizePath,
  textDirection,
} from '../../shared/i18n/locale';
import { I18nContext } from '../../shared/i18n/messages';

export interface LocaleInfo {
  /** The current page's locale; the default locale when i18n is off. */
  locale: string;
  defaultLocale: string;
  locales: readonly string[];
  dir: 'ltr' | 'rtl';
  /** The current path without its locale prefix. */
  path: string;
  /** Whether the current path exists in every locale. */
  localized: boolean;
  /** The path of a locale-free path in the current locale, or in `locale`. */
  localize: (path: string, locale?: string) => string;
}

function useCurrentPath(fallback: string): string {
  // Inside a router the location is already relative to the locale basename.
  // The payload path is normalized, so the router path is too: an island
  // hydrates without a router and must render the same links.
  const inRouter = useInRouterContext();
  // biome-ignore lint/correctness/useHookAtTopLevel: the router context of a tree never changes
  return normalizePath(inRouter ? useLocation().pathname : fallback);
}

/** The locale of the current page and helpers to build links in other locales. */
export function useLocale(): LocaleInfo {
  const state = useContext(I18nContext);
  const path = useCurrentPath(state?.path ?? '/');
  if (!state) {
    return {
      locale: 'en',
      defaultLocale: 'en',
      locales: ['en'],
      dir: 'ltr',
      path,
      localized: false,
      localize: (target) => target,
    };
  }
  const { settings, locale } = state;
  return {
    locale,
    defaultLocale: settings.defaultLocale,
    locales: settings.locales,
    dir: textDirection(locale),
    path,
    localized: isLocalizedPath(settings, path),
    localize: (target, targetLocale = locale) => localizePath(settings, target, targetLocale),
  };
}

/**
 * Record an explicit locale choice from the browser; the server reads it when a
 * reader opens `/`. A locale link needs none of this: its `?locale=` query
 * records the choice on the server.
 */
export function rememberLocale(locale: string): void {
  if (typeof document === 'undefined') return;
  const secure = typeof location !== 'undefined' && location.protocol === 'https:';
  // biome-ignore lint/suspicious/noDocumentCookie: a first-party preference cookie the server must read
  document.cookie = localeCookie(locale, secure);
}

export interface LocaleLink {
  locale: string;
  /** The locale's own name: `Français`. */
  label: string;
  /** The page in that locale, with the `?locale=` query that records the choice. */
  href: string;
  current: boolean;
}

/**
 * One link per published locale, to the current page in that locale, or to the
 * locale's home when the page exists in the default locale only.
 */
export function useLocaleLinks(): LocaleLink[] {
  const info = useLocale();
  // A page served in the default locale only links the other locales to their home.
  const target = (locale: string) => (info.localized || locale === info.locale ? info.path : '/');
  return info.locales.map((locale) => ({
    locale,
    label: localeEndonym(locale),
    href: `${info.localize(target(locale), locale)}?${LOCALE_PARAM}=${encodeURIComponent(locale)}`,
    current: locale === info.locale,
  }));
}

export interface LocaleSwitcherProps {
  /** Accessible name of the navigation landmark, in the current locale. */
  label: string;
  className?: string;
  /** Custom markup: receives the links, keeps their attributes your concern. */
  children?: (links: LocaleLink[]) => ReactNode;
}

/**
 * Plain links to the current page in every published locale: each one records
 * the reader's choice through its query, with or without JavaScript, and
 * carries its language so a screen reader pronounces the locale's name
 * correctly. Renders nothing when
 * the application publishes a single locale.
 */
export function LocaleSwitcher({ label, className, children }: LocaleSwitcherProps) {
  const links = useLocaleLinks();
  // One locale offers no choice: nothing to switch to.
  if (links.length < 2) return null;
  if (children) return <>{children(links)}</>;
  return (
    <nav aria-label={label} className={className}>
      <ul>
        {links.map((link) => (
          <li key={link.locale}>
            <a
              href={link.href}
              hrefLang={link.locale}
              lang={link.locale}
              aria-current={link.current ? 'page' : undefined}
            >
              {link.label}
            </a>
          </li>
        ))}
      </ul>
    </nav>
  );
}
