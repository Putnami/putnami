import React, { useContext } from 'react';
import {
  createPath,
  Link as RouterLink,
  type LinkProps as RouterLinkProps,
  useInRouterContext,
  useLocation,
} from 'react-router';
import { isLocalizedPath, localeHref, splitLocalePath, translatePath } from '../../shared/i18n/locale';
import { I18nContext, type I18nState } from '../../shared/i18n/messages';
import { type PrefetchBehavior, usePrefetch } from '../hooks/use-prefetch.hook';

export type LinkProps = RouterLinkProps & { prefetch?: PrefetchBehavior };

/**
 * A link to the current location, including one that changes only the query or
 * the hash, navigates like any other: React Router replaces the entry and
 * re-runs the loaders of a same-URL navigation.
 *
 * Outside a router (an island, a component test), the link renders a plain
 * `<a href>` with the same anchor props.
 *
 * In a localized application an absolute path stays in the current locale:
 * the router prefixes it on a hydrated page, and the plain link does the same.
 * A path served in the default locale only (`i18n.exclude`), and a path that
 * names a locale (`/fr/pricing`), link to it as written.
 */
export const Link = React.forwardRef<HTMLAnchorElement, LinkProps>((props, ref) =>
  useInRouterContext() ? <RoutedLink {...props} ref={ref} /> : <PlainLink {...props} ref={ref} />,
);

Link.displayName = 'Link';

const RoutedLink = React.forwardRef<HTMLAnchorElement, LinkProps>(({ to, prefetch = 'none', ...props }, ref) => {
  const { load } = usePrefetch(to, prefetch);
  const location = useLocation();

  // `to="#"` and an empty `to` mark a placeholder link: it points at the
  // current path, runs its own `onClick`, and does not navigate, because a
  // navigation would drop the current query and hash and re-run the loaders.
  const isPlaceholder = to === '#' || !to;
  if (isPlaceholder) {
    to = location.pathname;
  }

  const i18n = useContext(I18nContext);
  const href = typeof to === 'string' ? to : createPath(to);
  if (leavesLocale(i18n, href)) {
    // The router would put the current locale prefix in front of it; that page does not exist.
    return <PlainLink {...props} ref={ref} to={href} />;
  }

  // The router adds the locale prefix; the path is the one the locale names.
  if (i18n && !isPlaceholder) to = translatedTo(i18n, to);

  const mustPreload = prefetch === 'intent' && !to.toString().startsWith('http');

  const handleClick = (e: React.MouseEvent<HTMLAnchorElement>) => {
    props.onClick?.(e);
    if (isPlaceholder) {
      e.preventDefault();
    }
  };

  const handleMouseEnter = (e: React.MouseEvent<HTMLAnchorElement>) => {
    if (mustPreload) {
      load();
    }
    props.onMouseEnter?.(e);
  };

  const handleFocus = (e: React.FocusEvent<HTMLAnchorElement>) => {
    if (mustPreload) {
      load();
    }
    props.onFocus?.(e);
  };

  const handleTouchStart = (e: React.TouchEvent<HTMLAnchorElement>) => {
    if (mustPreload) {
      load();
    }
    props.onTouchStart?.(e);
  };
  return (
    <RouterLink
      {...props}
      ref={ref}
      to={to}
      onClick={handleClick}
      onMouseEnter={handleMouseEnter}
      onFocus={handleFocus}
      onTouchStart={handleTouchStart}
    />
  );
});

RoutedLink.displayName = 'RoutedLink';

const PlainLink = React.forwardRef<HTMLAnchorElement, LinkProps>(
  (
    {
      to,
      prefetch: _prefetch,
      discover: _discover,
      reloadDocument: _reloadDocument,
      replace: _replace,
      state: _state,
      preventScrollReset: _preventScrollReset,
      relative: _relative,
      viewTransition: _viewTransition,
      defaultShouldRevalidate: _defaultShouldRevalidate,
      mask: _mask,
      ...anchorProps
    },
    ref,
  ) => {
    const i18n = useContext(I18nContext);
    return <a {...anchorProps} ref={ref} href={localizedHref(i18n, typeof to === 'string' ? to : createPath(to))} />;
  },
);

PlainLink.displayName = 'PlainLink';

/** Split a site-absolute href into its path and its query plus hash. */
function splitHref(href: string): { path: string; rest: string } | undefined {
  if (!href.startsWith('/') || href.startsWith('//')) return undefined;
  const end = href.search(/[?#]/);
  return end === -1 ? { path: href, rest: '' } : { path: href.slice(0, end), rest: href.slice(end) };
}

/** Whether a site-absolute path already names a non-default locale. */
function namesLocale(i18n: I18nState, path: string): boolean {
  return splitLocalePath(i18n.settings, path).locale !== i18n.settings.defaultLocale;
}

/**
 * Whether a site-absolute link sits outside the router's locale basename: it
 * names a locale, or it targets a default-locale-only path from another locale.
 */
function leavesLocale(i18n: I18nState | undefined, href: string): boolean {
  const parts = i18n ? splitHref(href) : undefined;
  if (!i18n || !parts) return false;
  if (namesLocale(i18n, parts.path)) return true;
  return i18n.locale !== i18n.settings.defaultLocale && !isLocalizedPath(i18n.settings, parts.path);
}

/** A router `to` with its site-absolute path as the current locale names it: `/privacy` reads `/confidentialite` in French. */
function translatedTo(i18n: I18nState, to: LinkProps['to']): LinkProps['to'] {
  if (typeof to !== 'string') {
    return to.pathname?.startsWith('/')
      ? { ...to, pathname: translatePath(i18n.settings, to.pathname, i18n.locale) }
      : to;
  }
  const parts = splitHref(to);
  if (!parts || !isLocalizedPath(i18n.settings, parts.path)) return to;
  return `${translatePath(i18n.settings, parts.path, i18n.locale)}${parts.rest}`;
}

/**
 * The href of a site-absolute path in the current locale, absolute when
 * another origin serves it; a path that names a locale stays as written.
 */
function localizedHref(i18n: I18nState | undefined, href: string): string {
  const parts = i18n ? splitHref(href) : undefined;
  if (!i18n || !parts || namesLocale(i18n, parts.path)) return href;
  return `${localeHref(i18n.settings, parts.path, i18n.locale, i18n)}${parts.rest}`;
}
