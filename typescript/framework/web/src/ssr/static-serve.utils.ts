import { file } from 'bun';
import { cache, evictCache, type HttpRequestContext, type RouteHandler } from '@putnami/application';
import { joinPath } from '@putnami/utils';
import { useLogger } from '@putnami/runtime';
import type { StaticConfig, StaticPathParams } from './static';
import { staticHtmlPath } from './static';
import { readStaticBuildVersion, restampBuildVersion } from './static-build-version';
import { type StaticRenderResult, staticHtmlResponse, staticRedirectResponse } from './static-render';

const CACHE_NAME = 'web-static';

// ---------------------------------------------------------------------------
// ISR tag registry — maps a revalidation tag to the cache keys that carry it,
// so `revalidateTag('posts')` invalidates exactly the pages built from `posts`.
// ---------------------------------------------------------------------------

const tagRegistry = new Map<string, Set<string>>();

// Cache keys whose build output has already been served once. After the first
// serve, an ISR refresh renders fresh HTML rather than re-reading the (now
// stale) build output.
const seeded = new Set<string>();

// Tags revalidated at least once in this process. A tag is recorded here even
// when it currently has no registered cache keys, so a revalidation that
// happens BEFORE a page is first requested — e.g. a content-overlay swap on a
// cold, scaled-from-zero instance — still forces that page to render live on
// its first serve instead of caching now-stale build output for a full TTL.
const revalidatedTags = new Set<string>();

function registerTags(tags: readonly string[] | undefined, cacheKey: string): void {
  if (!tags) return;
  for (const tag of tags) {
    let set = tagRegistry.get(tag);
    if (!set) {
      set = new Set<string>();
      tagRegistry.set(tag, set);
    }
    set.add(cacheKey);
  }
}

/**
 * Revalidate every ISR page associated with `tag`, dropping its cached HTML so
 * the next request re-renders it. Mirrors Next's `revalidateTag` and reuses the
 * framework's existing tag/eviction cache.
 *
 * @returns the number of cached pages evicted.
 */
export async function revalidateTag(tag: string): Promise<number> {
  // Mark the tag stale first — before the early return — so a revalidation
  // with no cached pages yet still suppresses build output on later cold serves.
  revalidatedTags.add(tag);
  const keys = tagRegistry.get(tag);
  if (!keys || keys.size === 0) return 0;
  let evicted = 0;
  for (const key of keys) {
    evicted += await evictCache(`${CACHE_NAME}:${key}`);
  }
  return evicted;
}

/** Test helper: clear the in-memory tag registry and seed tracking. */
export function clearTagRegistry(): void {
  tagRegistry.clear();
  seeded.clear();
  revalidatedTags.clear();
}

/**
 * Whether any of `tags` has been revalidated in this process. Once a tag is
 * revalidated its pages must render live rather than serve build output — even
 * on a first serve — because the build artifact is stale relative to the
 * revalidated content (e.g. an active runtime content overlay).
 */
function tagsRevalidated(tags: readonly string[] | undefined): boolean {
  return tags?.some((tag) => revalidatedTags.has(tag)) ?? false;
}

/** Test helper: read the cache keys registered under a tag. */
export function tagsFor(tag: string): readonly string[] {
  return [...(tagRegistry.get(tag) ?? [])];
}

// ---------------------------------------------------------------------------
// Static serve handler
// ---------------------------------------------------------------------------

/** A resolved page to serve: HTML (with status), or a loader-driven redirect. */
interface ServedPage {
  html: string;
  status: number;
  /** Set when the route resolved to a redirect (e.g. a legacy-URL redirect). */
  redirect?: string;
}

interface StaticServeOptions {
  /** The React-Router route pattern (e.g. `/tasks/:id`) for diagnostics + render. */
  route: string;
  staticConfig: StaticConfig;
  /** Absolute directory holding the pre-rendered HTML files. */
  staticDir: string;
  /** App basename, when mounted under a path prefix. */
  basename?: string;
  /** Live (re-)render used for ISR refresh and for paths missing from the build. */
  render: (pathname: string, params: StaticPathParams) => Promise<StaticRenderResult>;
  /**
   * Version of the running build. A pre-rendered page is served with the
   * version it was rendered with replaced by this one; without it, the page is
   * served as rendered.
   */
  currentVersion?: () => string | undefined;
}

function pathnameFromContext(ctx: HttpRequestContext, basename?: string): string {
  let pathname: string;
  try {
    pathname = new URL(ctx.req.url).pathname;
  } catch {
    pathname = '/';
  }
  if (basename && basename !== '/' && pathname.startsWith(basename)) {
    pathname = pathname.slice(basename.length) || '/';
    if (!pathname.startsWith('/')) pathname = `/${pathname}`;
  }
  return pathname;
}

/**
 * Read a pre-rendered page and restamp the build version it was rendered with
 * to the running build's version: a cache restore or a container can serve
 * pages rendered at another commit.
 */
async function readPrebuilt(
  staticDir: string,
  pathname: string,
  restamp: (html: string) => string,
): Promise<string | undefined> {
  const filePath = joinPath(staticDir, staticHtmlPath(pathname));
  const handle = file(filePath);
  if (!(await handle.exists())) return undefined;
  return restamp(await handle.text());
}

/**
 * Build the runtime handler for a `.static()` route.
 *
 * - **SSG**: serves the pre-rendered HTML file. If the requested path was not
 *   enumerated at build, it is rendered live (and not cached).
 * - **ISR (`seconds`)**: serves the pre-rendered HTML, treated as a cache entry
 *   with a TTL; once stale, the next request re-renders and replaces it.
 * - **ISR (`tags`)**: the entry is registered under its tags so
 *   {@link revalidateTag} can evict it on demand.
 *
 * Static output is always zero-JavaScript (the same render path as the build).
 */
export function staticServeHandler(options: StaticServeOptions): RouteHandler {
  const { route, staticConfig, staticDir, basename, render, currentVersion } = options;
  const isIsr = staticConfig.mode === 'isr';
  const ttlMs = staticConfig.revalidate?.seconds ? staticConfig.revalidate.seconds * 1000 : undefined;
  const tags = staticConfig.revalidate?.tags;
  // The record is read on the first pre-rendered serve: the build output may
  // not exist yet when the handler is created.
  let renderedVersion: { value: string | undefined } | undefined;
  const restamp = (html: string) => {
    renderedVersion ??= { value: readStaticBuildVersion(staticDir) };
    return restampBuildVersion(html, renderedVersion.value, currentVersion?.());
  };

  const toResponse = (served: ServedPage) =>
    served.redirect !== undefined
      ? staticRedirectResponse(served.redirect, served.status)
      : staticHtmlResponse(served.html, served.status);

  return async (ctx) => {
    const pathname = pathnameFromContext(ctx, basename);
    const params = (ctx.params ?? {}) as StaticPathParams;

    const produce = async (): Promise<ServedPage> => {
      const prebuilt = await readPrebuilt(staticDir, pathname, restamp);
      if (prebuilt !== undefined) {
        return { html: prebuilt, status: 200 };
      }
      // Not pre-rendered (e.g. a legacy URL the loader redirects, or a path
      // missing from `paths()`): render live and honour any loader redirect.
      const result = await render(pathname, params);
      return { html: result.html, status: result.status, redirect: result.redirectLocation };
    };

    if (!isIsr) {
      return toResponse(await produce());
    }

    // ISR: cache the served HTML and re-render it once the TTL lapses or a tag
    // is revalidated. On a TTL refresh we re-render live so the page stays fresh.
    const cacheKey = `${route}:${pathname}`;
    registerTags(tags, cacheKey);

    const refresh = async (): Promise<ServedPage> => {
      try {
        const result = await render(pathname, params);
        return { html: result.html, status: result.status, redirect: result.redirectLocation };
      } catch (error) {
        useLogger('@putnami/web').warn(`ISR re-render failed for ${pathname}; serving build output`, error);
        return produce();
      }
    };

    let builder = cache(CACHE_NAME).for(cacheKey);
    if (ttlMs) builder = builder.ttl(ttlMs);
    const served = await builder.fetch<ServedPage>(async () => {
      // First serve: use the build output, but only while the page's tags are
      // still fresh. Every later cache miss (TTL lapse or tag revalidation), and
      // any first serve after one of the page's tags was already revalidated,
      // renders fresh so stale build HTML is never (re-)served.
      if (!seeded.has(cacheKey) && !tagsRevalidated(tags)) {
        const prebuilt = await readPrebuilt(staticDir, pathname, restamp);
        if (prebuilt !== undefined) {
          seeded.add(cacheKey);
          return { html: prebuilt, status: 200 };
        }
      }
      seeded.add(cacheKey);
      return refresh();
    });

    return toResponse(served);
  };
}
