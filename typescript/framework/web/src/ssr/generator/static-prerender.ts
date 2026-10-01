import { existsSync, mkdirSync } from 'node:fs';
import { isAbsolute, relative, resolve, sep } from 'node:path';
import { write } from 'bun';
import type { Token } from '@putnami/runtime';
import { type BuildInfo, getBuildInfo, getDirectoryName } from '@putnami/utils';
import { ISLAND_TAG } from '../../client/island/island-types';
import { isLoaderDefinition, type LoaderInjectMeta } from '../loader';
import { isPageDefinition } from '../page';
import type { ReactApplication } from '../react-application';
import { fillRoutePath, isDynamicRoute, isStaticRenderViolation, type StaticConfig, staticHtmlPath } from '../static';
import { restampableVersion, writeStaticBuildVersion } from '../static-build-version';

/** A route discovered to be `.static()` at build time. */
export interface StaticRouteSpec {
  /** React-Router style route, e.g. `/tasks/:id`. */
  route: string;
  staticConfig: StaticConfig;
}

/** A layout loader that may contribute DI roots to descendant routes. */
export interface LayoutLoaderCandidate {
  /** React-Router style route for the layout, e.g. `/dashboard`. */
  route: string;
  /** Absolute path to the layout loader module. */
  absPath: string;
}

/** One emitted static HTML file. */
interface PrerenderedPage {
  route: string;
  pathname: string;
  mode: 'ssg' | 'isr';
  status: number;
  bytes: number;
  /** Output path relative to the static output directory. */
  file: string;
  /** Whether the rendered HTML contains any island boundaries. */
  hasIslands: boolean;
}

/**
 * Load a `page.tsx` module at build time and return its static configuration,
 * if it declared one via `page().static()`. Import failures are swallowed —
 * an unresolvable page simply renders as SSR.
 */
export async function detectStaticConfig(absFilePath: string): Promise<StaticConfig | undefined> {
  try {
    const mod = (await import(absFilePath)) as Record<string, unknown>;
    const def = mod['default'] ?? mod['page'];
    if (isPageDefinition(def) && def.static) {
      return def.static;
    }
  } catch {
    // Unresolvable / throwing module → treat as non-static.
  }
  return undefined;
}

/**
 * Load a co-located `loader.ts` at build time and return the DI roots it
 * injects, if any. Used to feed the static-safety proof for `.static()` routes.
 * Missing loaders contribute no roots. Existing loaders that cannot be imported
 * are conservative hybrids because their DI roots cannot be proven.
 */
export async function detectLoaderInject(absFilePath: string): Promise<LoaderInjectMeta | undefined> {
  if (!existsSync(absFilePath)) {
    return undefined;
  }
  try {
    const mod = (await import(absFilePath)) as Record<string, unknown>;
    const def = mod['default'] ?? mod['loader'];
    if (isLoaderDefinition(def)) {
      return def.inject ?? { tokens: [], dynamic: false };
    }
  } catch {
    // Existing but unresolvable/throwing loader → hybrid proof.
    return { tokens: [], dynamic: true };
  }
  return { tokens: [], dynamic: true };
}

export function layoutAppliesToRoute(layoutRoute: string, route: string): boolean {
  return layoutRoute === '/' || route === layoutRoute || route.startsWith(`${layoutRoute}/`);
}

export function mergeLoaderInject(metas: readonly (LoaderInjectMeta | undefined)[]): LoaderInjectMeta {
  const tokens: Token[] = [];
  let dynamic = false;
  for (const meta of metas) {
    if (!meta) continue;
    tokens.push(...meta.tokens);
    dynamic ||= meta.dynamic;
  }
  return { tokens, dynamic };
}

function staticOutputPath(staticDir: string, rel: string): string {
  const root = resolve(staticDir);
  const outFile = resolve(root, rel);
  const pathFromRoot = relative(root, outFile);
  if (pathFromRoot === '..' || pathFromRoot.startsWith(`..${sep}`) || isAbsolute(pathFromRoot)) {
    throw new Error(`Static output path escapes static directory: ${rel}`);
  }
  return outFile;
}

/**
 * Return the combined DI roots for the route's page loader and every matching
 * ancestor layout loader. Static prerender executes all of these loaders.
 */
export async function detectRouteLoaderInject(
  route: string,
  pageLoaderPath: string,
  layoutLoaders: readonly LayoutLoaderCandidate[],
): Promise<LoaderInjectMeta> {
  const matchingLayoutLoaders = layoutLoaders
    .filter((layout) => layoutAppliesToRoute(layout.route, route))
    .map((layout) => layout.absPath);
  const metas = await Promise.all([...matchingLayoutLoaders, pageLoaderPath].map((path) => detectLoaderInject(path)));
  return mergeLoaderInject(metas);
}

/**
 * Pre-render every `.static()` route to a zero-JavaScript HTML file.
 *
 * A {@link StaticRenderViolation} (a static route touching request data) is a
 * hard build error and propagates. Any other render failure is logged and the
 * route is left to render live at runtime, so an individual page can never
 * break the whole build.
 *
 * When a page shows the build version the loaders read (`buildInfo`), that
 * version is recorded beside the pages, so the server can serve them under its
 * own version (see `static-build-version.ts`).
 */
export async function prerenderStaticRoutes(
  app: ReactApplication,
  specs: StaticRouteSpec[],
  staticDir: string,
  log?: (msg: string) => void,
  buildInfo: BuildInfo | undefined = getBuildInfo(),
): Promise<PrerenderedPage[]> {
  const pages: PrerenderedPage[] = [];
  const version = restampableVersion(buildInfo);
  let pagesShowVersion = false;

  for (const { route, staticConfig } of specs) {
    const dynamic = isDynamicRoute(route);

    let paramSets: Record<string, string>[];
    if (staticConfig.paths) {
      paramSets = await staticConfig.paths();
    } else if (dynamic) {
      log?.(`Skipping dynamic static route ${route}: no paths() enumeration provided`);
      continue;
    } else {
      paramSets = [{}];
    }

    for (const params of paramSets) {
      const pathname = dynamic ? fillRoutePath(route, params) : route;
      try {
        const result = await app.prerender(pathname, params, route);
        if (result.redirectLocation || !result.html) {
          log?.(`Skipping ${pathname}: resolved to status ${result.status}`);
          continue;
        }
        const rel = staticHtmlPath(pathname);
        const outFile = staticOutputPath(staticDir, rel);
        mkdirSync(getDirectoryName(outFile), { recursive: true });
        await write(outFile, result.html);
        pagesShowVersion ||= version !== undefined && result.html.includes(version);
        pages.push({
          route,
          pathname,
          mode: staticConfig.mode,
          status: result.status,
          bytes: Buffer.byteLength(result.html),
          file: rel,
          hasIslands: result.html.includes(`<${ISLAND_TAG}`),
        });
        log?.(`Pre-rendered ${pathname} → ${rel} (${result.status})`);
      } catch (err) {
        if (isStaticRenderViolation(err)) throw err;
        log?.(`Failed to pre-render ${pathname}: ${err}; will render live at runtime`);
      }
    }
  }

  writeStaticBuildVersion(staticDir, pagesShowVersion ? version : undefined);
  return pages;
}
