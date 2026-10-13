import { useCallback, useContext, useEffect, useRef } from 'react';
import { matchRoutes, parsePath, type To, UNSAFE_DataRouterContext, useHref } from 'react-router';
import { escapeCssAttrValue } from '../document/tag.utils';
import { useFetcher } from './use-fetcher.hook';
import { useResolvedPath } from './use-resolved-path.hook';

export type PrefetchBehavior = 'intent' | 'render' | 'none';

/**
 * Prefetches a route's code and data when triggered.
 *
 * It loads the route modules and warms the JSON loader endpoint
 * (`{basename}{path}.json{query}`) so that client-side navigation has both
 * ready. It does not run the route's loader: a loader that redirects would make
 * React Router navigate on a hover.
 */
export function usePrefetch(
  to: To,
  prefetch: PrefetchBehavior = 'none',
): {
  load: () => void;
  /** @deprecated The prefetch no longer loads this fetcher; it stays idle. */
  fetcher: ReturnType<typeof useFetcher>;
} {
  const fetcher = useFetcher();
  const path = useResolvedPath(to);
  const href = useHref(to);
  const router = useContext(UNSAFE_DataRouterContext)?.router;
  const jsonPrefetched = useRef(false);

  const load = useCallback(() => {
    // The route modules are lazy: importing them now leaves the navigation
    // nothing to download. React Router later imports the same cached chunks.
    for (const match of (router && matchRoutes(router.routes, path.pathname)) ?? []) {
      const lazy = match.route.lazy;
      if (typeof lazy === 'function') {
        Promise.resolve(lazy()).catch(() => undefined);
      }
    }

    // Also prefetch the JSON loader endpoint so data is cached
    if (!jsonPrefetched.current) {
      jsonPrefetched.current = true;
      // The URL the client loader fetches on navigation: the mounted basename,
      // the path with its `.json` suffix, then the query string.
      const target = parsePath(href);
      const jsonUrl = `${target.pathname ?? ''}.json${target.search ?? ''}`;
      const link = document.querySelector(`link[href="${escapeCssAttrValue(jsonUrl)}"]`);
      if (!link) {
        const prefetchLink = document.createElement('link');
        prefetchLink.rel = 'prefetch';
        prefetchLink.as = 'fetch';
        prefetchLink.crossOrigin = 'anonymous';
        prefetchLink.href = jsonUrl;
        document.head.appendChild(prefetchLink);
      }
    }
  }, [router, path, href]);

  useEffect(() => {
    if (prefetch === 'render') {
      load();
    }
  }, [prefetch, load]);

  return {
    load,
    fetcher,
  };
}
