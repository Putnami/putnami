/**
 * Client-side index loader.
 * Fetches /search/index.json once, caches in module scope.
 */
import type { SearchIndex } from './types';

let cachedIndex: SearchIndex | null = null;
let loadPromise: Promise<SearchIndex> | null = null;

/** Fetch and cache the search index. Subsequent calls return the cached value. */
export function loadSearchIndex(): Promise<SearchIndex> {
  if (cachedIndex) return Promise.resolve(cachedIndex);
  if (loadPromise) return loadPromise;

  loadPromise = fetch('/search/index.json')
    .then((res) => {
      if (!res.ok) throw new Error(`Failed to load search index: ${res.status}`);
      return res.json() as Promise<SearchIndex>;
    })
    .then((index) => {
      cachedIndex = index;
      return index;
    })
    .catch((err) => {
      loadPromise = null;
      throw err;
    });

  return loadPromise;
}
