import type { RouteObject } from 'react-router';
import { type LocaleSettings, normalizePath } from './locale';

function childPath(parent: string, path: string | undefined): string {
  if (!path) return parent;
  if (path.startsWith('/')) return normalizePath(path);
  return normalizePath(parent === '/' ? `/${path}` : `${parent}/${path}`);
}

/**
 * The route tree a locale renders: each page its locale translates gains a
 * sibling at the translated path, with the same page, loader and action. The
 * server and the browser build the same tree from the same settings, so the
 * render and the hydration match. Nodes on no translated path are shared, not
 * copied.
 */
export function localeRoutes(routes: RouteObject[], settings: LocaleSettings, locale: string): RouteObject[] {
  const translations = settings.paths[locale];
  if (!translations) return routes;
  const visit = (node: RouteObject, parent: string): RouteObject => {
    if (!node.children) return node;
    const full = childPath(parent, node.path);
    let changed = false;
    const children: RouteObject[] = [];
    for (const child of node.children) {
      const next = visit(child, full);
      if (next !== child) changed = true;
      children.push(next);
      const translated = child.children || child.index ? undefined : translations[childPath(full, child.path)];
      // Generation refuses a translated path outside its layout, so the prefix holds.
      if (translated?.startsWith(full === '/' ? '/' : `${full}/`)) {
        changed = true;
        const relative = translated.slice(full === '/' ? 1 : full.length + 1);
        children.push({ ...child, id: `${child.id ?? relative}@${locale}`, path: relative } as RouteObject);
      }
    }
    return changed ? ({ ...node, children } as RouteObject) : node;
  };
  return routes.map((route) => visit(route, '/'));
}
