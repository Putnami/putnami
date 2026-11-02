import { HttpResponse, type RouteHandler } from '@putnami/application';
import { file } from 'bun';
import { escapeHtml } from '../../client/document/tag.utils';
import { isLocalizedPath, type LocaleSettings, localizedUrl, splitLocalePath } from '../../shared/i18n/locale';

/** The file the build writes in the public output, served at `/sitemap.xml`. */
export const SITEMAP_FILE = 'sitemap.xml';

export interface SitemapPage {
  /** The page's route, without a locale prefix: `/pricing`, `/docs/*`. */
  route: string;
  /** The paths pre-rendered for the route, in every locale. */
  prerendered: readonly string[];
}

/** Whether a route matches more than one path. */
function hasParameters(route: string): boolean {
  return route.includes(':') || route.includes('*');
}

/**
 * The sitemap of a localized application: one URL per page route without
 * parameters and per pre-rendered path, in each locale the route is served in.
 * A localized URL lists an alternate per published locale and `x-default`,
 * as its page head does. URLs are sorted, so the same routes write the same
 * bytes.
 */
export function buildSitemap(settings: LocaleSettings, pages: readonly SitemapPage[]): string {
  const paths = new Set<string>();
  for (const page of pages) {
    if (!hasParameters(page.route)) paths.add(page.route);
    else for (const path of page.prerendered) paths.add(splitLocalePath(settings, path).path);
  }
  const entries: { loc: string; alternates: string[] }[] = [];
  for (const path of paths) {
    if (!isLocalizedPath(settings, path) || settings.locales.length < 2) {
      entries.push({ loc: localizedUrl(settings, path, settings.defaultLocale), alternates: [] });
      continue;
    }
    const alternates = [
      ...settings.locales.map((locale) => alternate(locale, localizedUrl(settings, path, locale))),
      alternate('x-default', localizedUrl(settings, path, settings.defaultLocale)),
    ];
    for (const locale of settings.locales) {
      entries.push({ loc: localizedUrl(settings, path, locale), alternates });
    }
  }
  entries.sort((a, b) => (a.loc < b.loc ? -1 : a.loc > b.loc ? 1 : 0));
  const urls = entries.map(({ loc, alternates }) =>
    ['  <url>', `    <loc>${escapeHtml(loc)}</loc>`, ...alternates, '  </url>'].join('\n'),
  );
  return [
    '<?xml version="1.0" encoding="UTF-8"?>',
    '<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9" xmlns:xhtml="http://www.w3.org/1999/xhtml">',
    ...urls,
    '</urlset>',
    '',
  ].join('\n');
}

function alternate(hreflang: string, href: string): string {
  return `    <xhtml:link rel="alternate" hreflang="${escapeHtml(hreflang)}" href="${escapeHtml(href)}"/>`;
}

/** Serve the sitemap the build wrote; 404 before any build wrote one. */
export function sitemapHandler(path: string): RouteHandler {
  return async () => {
    const sitemap = file(path);
    if (!(await sitemap.exists())) return new HttpResponse('', { status: 404 });
    return new HttpResponse(await sitemap.text(), { headers: { 'content-type': 'application/xml; charset=utf-8' } });
  };
}
