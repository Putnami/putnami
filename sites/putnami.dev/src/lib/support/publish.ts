/**
 * Publish the generated `/docs/support` page into the docs tree.
 *
 * Idempotent by construction: the page is a pure function of the reviewed
 * `putnami.support.json`, so running this twice in one build writes identical
 * bytes. That matters because it IS run twice, for the same reason content
 * bundles are:
 *
 * - the web generate hook asks the docs page for its static paths BEFORE the
 *   application generate hook runs, so the page must already be on disk or a
 *   cold build would enumerate one page fewer than every later build and emit a
 *   different `putnami.http-routes.v1` digest;
 * - the application generate hook then runs again to register the file as a
 *   build asset, because a file written into `.gen` and never registered is not
 *   copied to `dist/` and 404s in the packaged image.
 *
 * The page is written ONLY under `.gen/public/docs`. Mirroring it into the
 * source `public/docs` would make the staticFiles plugin declare a route the
 * public-surface plugin also declares, which fails the route inventory with
 * `http_routes.duplicate_route` (see `assertNoSourceMirror`). The docs nav and
 * loader both read `.gen/public/docs`, so dev serve resolves it there too.
 *
 * Server-only (filesystem): never import it from a client bundle.
 */
import { mkdirSync, writeFileSync } from 'node:fs';
import { dirname } from 'node:path';
import { getProjectRoot, getWorkspaceRoot, joinPath } from '@putnami/utils';
import { renderSupportPage } from './catalog';
import { readSupportCatalog } from './catalog.server';

/**
 * Docs section hosting the generated page. The `NN-` prefix is what the docs
 * nav scanner reads to order the section and derive the `/docs/support` URL,
 * exactly as for a site-local section. No `generate.assets` target and no
 * content-bundle mount owns this prefix.
 */
export const SUPPORT_SECTION = '10-support';

/** Build-output-relative path of the generated page. */
export const SUPPORT_PAGE_DIST_PATH = joinPath('public', 'docs', SUPPORT_SECTION, 'index.md');

export interface PublishSupportPageOptions {
  projectRoot?: string;
  workspaceRoot?: string;
}

export interface PublishedSupportPage {
  /** Generate assets to return from a plugin, keyed by dist-relative path. */
  assets: Record<string, string>;
  /** Absolute path written under `.gen`. */
  genPath: string;
}

/**
 * Render the reviewed catalog and write the page to both roots.
 *
 * An unreadable or invalid catalog throws, failing the build. Publishing a page
 * that silently omitted reviewed statuses would understate a public commitment,
 * which is worse than not building.
 */
export function publishSupportPage(options: PublishSupportPageOptions = {}): PublishedSupportPage {
  const projectRoot = options.projectRoot ?? getProjectRoot();
  const workspaceRoot = options.workspaceRoot ?? getWorkspaceRoot();
  const page = renderSupportPage(readSupportCatalog(workspaceRoot));

  const genPath = joinPath(projectRoot, '.gen', SUPPORT_PAGE_DIST_PATH);
  mkdirSync(dirname(genPath), { recursive: true });
  writeFileSync(genPath, page);

  return { assets: { [SUPPORT_PAGE_DIST_PATH]: genPath }, genPath };
}
