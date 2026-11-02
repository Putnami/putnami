import { type HttpRequestContext, HttpResponse, type RequestRewrite, type RouteHandler } from '@putnami/application';
import {
  domainLocale,
  LOCALE_PARAM,
  type LocaleServing,
  type LocaleSettings,
  localeCookie,
  localeHref,
  localizedUrl,
  negotiateLocale,
  preferredLocale,
  publishedLocale,
  splitLocalePath,
} from '../../shared/i18n/locale';

/** What the entry response depends on, for every cache between it and the reader. */
const ENTRY_VARY = 'Accept-Language, Cookie';

/**
 * Send a reader who opens the site root to their locale: the locale they chose
 * in the switcher (its cookie) first, then their browser's `Accept-Language`,
 * then the default locale, which renders in place. Only the root negotiates;
 * every other URL names its locale and is served as it is, so a shared link
 * and a crawler always see the page they asked for.
 */
export function negotiatingEntry(handler: RouteHandler, settings: LocaleSettings): RouteHandler {
  return async (ctx) => {
    const { headers, url } = ctx.req;
    const locale =
      preferredLocale(settings, headers.get('cookie')) ??
      negotiateLocale(settings, headers.get('accept-language')) ??
      settings.defaultLocale;
    if (locale !== settings.defaultLocale) {
      return new HttpResponse('', {
        status: 302,
        headers: {
          location: `${localeHref(settings, '/', locale, { locale: settings.defaultLocale, served: serving(ctx, settings) })}${new URL(url).search}`,
          vary: ENTRY_VARY,
        },
      });
    }
    const response = await handler(ctx);
    if (response instanceof HttpResponse) {
      const vary = response.getHeader('vary');
      response.setHeader('vary', vary ? `${vary}, ${ENTRY_VARY}` : ENTRY_VARY);
    }
    return response;
  };
}

/**
 * Record the choice a locale link carries, `/pricing?locale=fr`, in the
 * preference cookie, and send the reader to the same URL without it. A link
 * records the choice this way on every page, with or without JavaScript. The
 * answer sets a cookie, so no cache keeps it. A tag that names no published
 * locale is ignored.
 */
export function recordingChoice(handler: RouteHandler, settings: LocaleSettings): RouteHandler {
  return (ctx) => {
    const url = new URL(ctx.req.url);
    const tag = url.searchParams.get(LOCALE_PARAM);
    const locale = tag === null ? undefined : publishedLocale(settings, tag);
    if (!locale) return handler(ctx);
    url.searchParams.delete(LOCALE_PARAM);
    return new HttpResponse('', {
      status: 302,
      headers: {
        location: `${url.pathname}${url.search}`,
        'set-cookie': localeCookie(locale, ctx.secured()),
        'cache-control': 'no-store',
      },
    });
  };
}

/**
 * Send a reader who adds a locale prefix to a route outside the localized set,
 * such as `/fr/docs/intro`, to its one URL, `/docs/intro`, with the query; on
 * the locale's domain, to that URL on the site. 308 keeps the method and tells
 * caches and crawlers the move is permanent.
 */
export function excludedRouteRedirect(prefix: string, settings: LocaleSettings): RouteHandler {
  return (ctx) => {
    const url = new URL(ctx.url);
    const path = url.pathname.slice(prefix.length) || '/';
    const site = onItsDomain(ctx, settings) ? settings.siteUrl : '';
    return new HttpResponse('', { status: 308, headers: { location: `${site}${path}${url.search}` } });
  };
}

/**
 * Send a reader from a path its locale translates, `/fr/privacy`, to the
 * translated one, `/fr/confidentialite`, with the query; on the locale's
 * domain, to `/confidentialite`. 308 keeps the method and tells caches and
 * crawlers the move is permanent.
 */
export function movedTo(path: string, settings: LocaleSettings): RouteHandler {
  return (ctx) => {
    const { search } = new URL(ctx.url);
    const target = onItsDomain(ctx, settings) ? path.slice(path.indexOf('/', 1)) || '/' : path;
    return new HttpResponse('', { status: 308, headers: { location: `${target}${search}` } });
  };
}

/**
 * Serve a locale's domain at its root: a request to that host is routed to
 * the locale's pages, `/pricing` to `/fr/pricing`. A path the locale does not
 * serve keeps its own route.
 */
export function localeDomainRewrite(settings: LocaleSettings): RequestRewrite {
  return (req, path) => {
    const locale = domainLocale(settings, new URL(req.url).host);
    if (!locale) return undefined;
    return path === '/' ? `/${locale}` : `/${locale}${path}`;
  };
}

/** Whether a request reached its locale's page through the locale's domain. */
function onItsDomain(ctx: HttpRequestContext, settings: LocaleSettings): boolean {
  const routed = new URL(ctx.url);
  const locale = domainLocale(settings, routed.host);
  return (
    locale !== undefined && ctx.url !== ctx.req.url && splitLocalePath(settings, routed.pathname).locale === locale
  );
}

/** Where a request's page is served: at its locale's domain, on the site's host, or elsewhere under its prefix. */
export function serving(ctx: HttpRequestContext, settings: LocaleSettings): LocaleServing {
  if (onItsDomain(ctx, settings)) return 'domain';
  return new URL(ctx.req.url).host === new URL(settings.siteUrl).host ? 'site' : 'prefix';
}

/**
 * Send a reader who opens a locale's page under its prefix on the site,
 * `https://example.com/fr/pricing`, to the locale's domain,
 * `https://example.fr/pricing`, with the query. Any other host, such as a
 * development server or a preview, serves the page under its prefix.
 */
export function servingOnDomain(handler: RouteHandler, settings: LocaleSettings): RouteHandler {
  const siteHost = new URL(settings.siteUrl).host;
  return (ctx) => {
    const url = new URL(ctx.req.url);
    if (url.host !== siteHost) return handler(ctx);
    const { locale, path } = splitLocalePath(settings, url.pathname);
    if (settings.domains[locale] === undefined) return handler(ctx);
    return new HttpResponse('', {
      status: 308,
      headers: { location: `${localizedUrl(settings, path, locale)}${url.search}` },
    });
  };
}
