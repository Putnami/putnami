import type React from 'react';
import { Outlet, type RouteObject, UNSAFE_useScrollRestoration } from 'react-router';

/**
 * Applies React Router's scroll restoration to the page it wraps: a new
 * location scrolls to the top or to its hash target, back and forward restore
 * the saved position, and `preventScrollReset` keeps the position.
 *
 * It renders its single child and no element of its own, so the server markup
 * and the hydrated tree keep the same shape.
 */
function ScrollRestoring({ children }: { children: React.ReactNode }): React.ReactNode {
  UNSAFE_useScrollRestoration();
  return children;
}

type RouteRendering = Pick<RouteObject, 'element' | 'Component'>;

function wrapRendering({ element, Component }: RouteRendering): RouteRendering {
  const content = element !== undefined ? element : Component ? <Component /> : <Outlet />;
  return { element: <ScrollRestoring>{content}</ScrollRestoring>, Component: undefined };
}

/**
 * Makes the root route restore scroll on client navigation. A lazy root route
 * gets the wrapper when it loads.
 *
 * @param route - The root route, changed in place.
 */
export function restoreScrollAtRoot(route: RouteObject): void {
  const { lazy } = route;
  if (typeof lazy === 'function') {
    route.lazy = async () => {
      const loaded = await lazy();
      return { ...loaded, ...wrapRendering(loaded) };
    };
    return;
  }
  Object.assign(route, wrapRendering(route));
}
