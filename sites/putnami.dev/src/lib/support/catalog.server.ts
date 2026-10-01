/**
 * Server-only access to the reviewed support catalog.
 *
 * Separated from `catalog.ts` so islands and client bundles can import the
 * vocabulary and the renderer without pulling `node:fs` into the browser graph.
 */
import { readFileSync } from 'node:fs';
import { getWorkspaceRoot, joinPath } from '@putnami/utils';
import { type SupportCatalog, parseSupportCatalog } from './catalog';

/** The reviewed catalog's filename, fixed by `protocols/support`. */
export const SUPPORT_CATALOG_FILENAME = 'putnami.support.json';

/** Workspace-root path of the reviewed catalog. */
export function supportCatalogPath(workspaceRoot: string = getWorkspaceRoot()): string {
  return joinPath(workspaceRoot, SUPPORT_CATALOG_FILENAME);
}

/**
 * Read and parse the reviewed catalog.
 *
 * A missing or invalid catalog throws. The site publishes these statuses as a
 * public commitment, so failing the build is the correct outcome — rendering an
 * empty or partial table would read as "nothing is supported".
 */
export function readSupportCatalog(workspaceRoot: string = getWorkspaceRoot()): SupportCatalog {
  const path = supportCatalogPath(workspaceRoot);
  let source: string;
  try {
    source = readFileSync(path, 'utf8');
  } catch (error) {
    throw new Error(`${SUPPORT_CATALOG_FILENAME}: cannot read ${path}: ${(error as Error).message}`);
  }
  return parseSupportCatalog(source);
}
