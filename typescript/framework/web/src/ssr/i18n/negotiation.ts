import { HttpResponse, type RouteHandler } from '@putnami/application';
import { type LocaleSettings, localizePath, negotiateLocale, preferredLocale } from '../../shared/i18n/locale';

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
        headers: { location: `${localizePath(settings, '/', locale)}${new URL(url).search}`, vary: ENTRY_VARY },
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
