import { matchRoutes, type RouteObject } from 'react-router';

let hydrated: { routes: RouteObject[]; basename?: string } | undefined;

/**
 * Records the routes of the hydrated router, so a redirect can tell whether
 * the router renders its target.
 *
 * @param routes - The routes the router was created with, or `undefined` to forget them.
 * @param basename - The router's basename.
 */
export function setRouterRoutes(routes: RouteObject[] | undefined, basename?: string): void {
  hydrated = routes ? { routes, basename } : undefined;
}

/**
 * Whether the hydrated router renders a page for `pathname`.
 *
 * A path outside the basename, a path no route matches, and a path only a
 * not-found catch-all matches are not router pages: the server owns them. A
 * catch-all page (`[...path]/page.tsx`) is a router page.
 *
 * @param pathname - The pathname to test, basename included.
 * @returns `undefined` when no router was hydrated.
 */
export function isRouterPage(pathname: string): boolean | undefined {
  if (!hydrated) {
    return undefined;
  }
  const matches = matchRoutes(hydrated.routes, pathname, hydrated.basename);
  const leaf = matches?.[matches.length - 1]?.route;
  if (!leaf) {
    return false;
  }
  return leaf.path !== '*' || leaf.id?.endsWith('-page') === true;
}
