import { readFileSync } from 'node:fs';
import { useLogger } from '@putnami/runtime';
import { fileExists, getProjectRoot, joinPath } from '@putnami/utils';

/** The build fragment `@putnami/web`'s pre-build hook writes. */
const FRAGMENT = ['.gen', 'http-routes.d', 'web.json'];

/** Only file routes are pages; API and static mounts are not navigable. */
const PAGE_SOURCE_KIND = 'file-route';

/** One route fact, reduced to the two members this reader needs. */
interface RouteFact {
  path?: unknown;
  provenance?: { sourceKind?: unknown };
}

/**
 * Converts a generated route template to the file-route spelling the browser
 * reports (`{id}` → `[id]`, `{path...}` → `[...path]`).
 *
 * The fragment is written in the v1 protocol spelling by
 * `normalizeGeneratedHttpRoutePath`, while the client router reports the
 * bracket form its file names are written in. Comparing the two spellings
 * without this conversion would file every parameterized page as unknown.
 *
 * @param path - The generated route path.
 * @returns The same route in file-route form.
 */
export function toFileRoute(path: string): string {
  return path.replace(/\{([A-Za-z0-9_]+)\.\.\.\}/g, '[...$1]').replace(/\{([A-Za-z0-9_]+)\}/g, '[$1]');
}

/**
 * Reads the bounded set of page routes a client event may name (body §E.4).
 *
 * The set is what keeps `route` a bounded counter dimension: a browser is free
 * to POST any string, so a value outside this set is stored as `__unknown__`
 * rather than minting a new series. An absent fragment — a test, or a dev run
 * that never generated — yields the empty set, which is the safe direction:
 * every client route folds under `__unknown__` and nothing unbounded is stored.
 *
 * @param projectRoot - The application root; defaults to the current project.
 * @returns The known page routes, in file-route form.
 */
export function loadKnownRoutes(projectRoot: string = getProjectRoot()): ReadonlySet<string> {
  const file = joinPath(projectRoot, ...FRAGMENT);
  if (!fileExists(file)) {
    useLogger('@putnami/analytics').debug(
      `analytics: no ${FRAGMENT.join('/')} fragment; every client route is recorded as __unknown__`,
    );
    return new Set();
  }
  let routes: RouteFact[];
  try {
    routes = (JSON.parse(readFileSync(file, 'utf8')) as { routes?: RouteFact[] }).routes ?? [];
  } catch (error) {
    useLogger('@putnami/analytics').warn(
      `analytics: could not read the web route fragment: ${error instanceof Error ? error.message : String(error)}`,
    );
    return new Set();
  }
  const known = new Set<string>();
  for (const route of routes) {
    if (route?.provenance?.sourceKind === PAGE_SOURCE_KIND && typeof route.path === 'string') {
      known.add(toFileRoute(route.path));
    }
  }
  return known;
}
