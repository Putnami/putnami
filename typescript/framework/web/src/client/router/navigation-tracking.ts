import type { RouterState } from 'react-router';
import { dispatchNavigation } from './navigation-event';

/**
 * Turns a router state into the file-route pattern of its matched leaf.
 *
 * react-router describes params as `:param` and a splat as `*`; every other
 * Putnami surface (file routes, route manifests, telemetry) uses `[param]` and
 * `[...rest]`. Normalizing here keeps a single vocabulary on the wire.
 */
export function routeOf(state: RouterState): string {
  const leaf = state.matches.at(-1);
  if (!leaf) return '__unknown__';
  const segments = state.matches
    .map((match) => match.route.path)
    .filter((path): path is string => !!path && path !== '/');
  const joined = `/${segments.join('/')}`.replace(/\/+/g, '/');
  return joined.replace(/:([A-Za-z0-9_]+)/g, '[$1]').replace(/\*$/, '[...rest]') || '/';
}

/** The location fields a tracker needs to prime itself. */
export interface NavigationOrigin {
  pathname: string;
  search: string;
}

/**
 * Builds the router subscriber that dispatches {@link dispatchNavigation} when
 * the location really moved.
 *
 * Priming with `origin` is what keeps the initial page load silent: the server
 * already recorded that view, so re-announcing it on hydration would double
 * count it. A settled navigation to the same `pathname + search` is also
 * ignored — react-router notifies on every state transition, not only on moves.
 */
export function createNavigationTracker(origin?: NavigationOrigin): (state: RouterState) => void {
  let lastKey = origin ? `${origin.pathname}${origin.search}` : undefined;
  let lastPathname = origin?.pathname;

  return (state: RouterState): void => {
    if (state.navigation.state !== 'idle') return;
    const key = `${state.location.pathname}${state.location.search}`;
    if (key === lastKey) return;
    const previous = lastPathname;
    lastKey = key;
    lastPathname = state.location.pathname;
    dispatchNavigation({
      pathname: state.location.pathname,
      search: state.location.search,
      hash: state.location.hash,
      route: routeOf(state),
      previous,
    });
  };
}
