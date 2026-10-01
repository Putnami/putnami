import { useCallback, useEffect, useRef } from 'react';
import type { To } from 'react-router';
import { escapeCssAttrValue } from '../document/tag.utils';
import { useFetcher } from './use-fetcher.hook';
import { useResolvedPath } from './use-resolved-path.hook';

export type PrefetchBehavior = 'intent' | 'render' | 'none';

/**
 * Prefetches a route's data when triggered.
 *
 * In addition to loading the route module (via React Router's fetcher),
 * this also warms the JSON loader endpoint (`{path}.json`) so that
 * client-side navigation has the data ready.
 */
export function usePrefetch(
  to: To,
  prefetch: PrefetchBehavior = 'none',
): { load: () => void; fetcher: ReturnType<typeof useFetcher> } {
  const fetcher = useFetcher();
  const path = useResolvedPath(to);
  const jsonPrefetched = useRef(false);

  const load = useCallback(() => {
    if (fetcher.state === 'idle' && !fetcher.data) {
      fetcher.load(path.pathname);
    }

    // Also prefetch the JSON loader endpoint so data is cached
    if (!jsonPrefetched.current) {
      jsonPrefetched.current = true;
      const jsonUrl = `${path.pathname}.json`;
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
  }, [fetcher, path]);

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
