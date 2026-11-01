import { type ReactNode, useContext } from 'react';
import { useInRouterContext, useLocation } from 'react-router';
import { isLocalizedPath, LOCALE_COOKIE, localeEndonym, localizePath, textDirection } from '../../shared/i18n/locale';
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
  const inRouter = useInRouterContext();
  // biome-ignore lint/correctness/useHookAtTopLevel: the router context of a tree never changes
  return inRouter ? useLocation().pathname : fallback;
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

/** Record an explicit locale choice; the server reads it when a reader opens `/`. */
export function rememberLocale(locale: string): void {
  if (typeof document === 'undefined') return;
  const secure = typeof location !== 'undefined' && location.protocol === 'https:' ? '; Secure' : '';
  // biome-ignore lint/suspicious/noDocumentCookie: a first-party preference cookie the server must read
  document.cookie = `${LOCALE_COOKIE}=${encodeURIComponent(locale)}; Path=/; Max-Age=31536000; SameSite=Lax${secure}`;
}

export interface LocaleLink {
  locale: string;
  /** The locale's own name: `Français`. */
  label: string;
  href: string;
  current: boolean;
  /** Records the choice; call it from the link's click handler. */
  select: () => void;
}

/**
 * One link per published locale, to the current page in that locale, or to the
 * locale's home when the page exists in the default locale only.
 */
export function useLocaleLinks(): LocaleLink[] {
  const info = useLocale();
  const target = info.localized ? info.path : '/';
  return info.locales.map((locale) => ({
    locale,
    label: localeEndonym(locale),
    href: info.localize(target, locale),
    current: locale === info.locale,
    select: () => rememberLocale(locale),
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
 * Plain links to the current page in every published locale: they work without
 * JavaScript, crawlers follow them, and each one carries its language so a
 * screen reader pronounces the locale's name correctly.
 */
export function LocaleSwitcher({ label, className, children }: LocaleSwitcherProps) {
  const links = useLocaleLinks();
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
              aria-current={link.current ? 'true' : undefined}
              onClick={link.select}
            >
              {link.label}
            </a>
          </li>
        ))}
      </ul>
    </nav>
  );
}
