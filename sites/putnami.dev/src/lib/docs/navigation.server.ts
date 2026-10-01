/**
 * Server-only navigation functions.
 * These use Bun APIs and cannot be imported in browser code.
 */
import { Glob } from 'bun';
import { useConfig, useLogger } from '@putnami/runtime';
import { getProjectRoot, joinPath } from '@putnami/utils';
import { SiteConfig } from '../../config';
import { activeContentDigest, deactivateOverlay, getActiveDocsRoot } from '../content/overlay';
import type { NavItem } from './navigation';

const NAV_CACHE_TTL_MS = 5 * 60 * 1000; // 5 minutes
const FIRST_PAGE_NAME = 'Getting Started';

let _navTreeCache: NavItem[] | undefined;
let _navTreeCacheExpiresAt = 0;

/**
 * List the markdown under the docs root every reader must resolve through,
 * degrading to baked content when an ACTIVE overlay scans empty.
 *
 * An empty overlay is never legitimate: every version is materialized from a
 * copy of the baked tree, so zero files means the version was damaged after it
 * was finalized — an OS temp reaper deleting its files and leaving the
 * directory tree is the observed case. Because the overlay REPLACES the baked
 * root rather than layering over it, leaving the pointer up would 404 every
 * docs page while a complete baked tree sits unread.
 *
 * Dropping the pointer (rather than just reading baked here) keeps every
 * docs-content reader consistent — the loader resolves file paths through the
 * same {@link getDocsRoot} — and lets the next newness check re-converge on
 * its own, since it compares against a now-null active digest.
 */
function scanDocsMarkdown(): string[] {
  const files = [...new Glob('**/*.md').scanSync({ cwd: getDocsRoot() })];
  if (files.length > 0 || activeContentDigest() === null) return files;

  const baked = [...new Glob('**/*.md').scanSync({ cwd: getBakedDocsRoot() })];
  // Both roots empty: nothing to degrade to, and nothing to blame on the overlay.
  if (baked.length === 0) return files;

  const digest = deactivateOverlay();
  useLogger('putnami.dev').warn(
    `content overlay: active version ${digest?.slice(0, 12)}… holds no markdown; serving baked content ` +
      `until the next newness check re-converges`,
  );
  return baked;
}

/**
 * Extract order number from a name like "01-introduction" or "02-concepts".
 * Returns { order, name } where name has the prefix stripped.
 */
function parseOrderedName(name: string): { order: number; name: string } {
  const match = name.match(/^(\d+)-(.+)$/);
  if (match) {
    return { order: parseInt(match[1], 10), name: match[2] };
  }
  return { order: 999, name };
}

/**
 * Normalize a name: replace dashes/underscores with spaces, capitalize first letter of each word.
 */
function normalizeName(name: string): string {
  return name.replace(/[-_]/g, ' ').replace(/\b\w/g, (char) => char.toUpperCase());
}

/**
 * Build navigation tree from generated public docs.
 *
 * Source of truth:
 *   .gen/public/docs/<package>/<topic>.md[.gz]
 * (overlaid by the runtime content overlay when one is active — see
 * {@link getDocsRoot})
 */
export async function getNavTree(_config = useConfig(SiteConfig)): Promise<NavItem[]> {
  if (_navTreeCache && Date.now() < _navTreeCacheExpiresAt) {
    return _navTreeCache;
  }

  const files = scanDocsMarkdown();
  files.sort();

  const nodeMap = new Map<string, NavItem>();
  const root: NavItem[] = [];

  for (const filePath of files) {
    const parts = filePath.split('/');

    // Skip files in hidden folders (starting with '.')
    if (parts.some((part) => part.startsWith('.'))) {
      continue;
    }

    const fileName = parts.pop()!;
    const isIndex = fileName === 'index.md';
    const fileBaseName = fileName.replace(/\.md$/, '');

    let currentLevel = root;
    let currentPath = '';

    // Build folder nodes
    for (const folderName of parts) {
      const folderPath = currentPath ? `${currentPath}/${folderName}` : folderName;

      if (!nodeMap.has(folderPath)) {
        const { order, name } = parseOrderedName(folderName);
        const normalizedName = normalizeName(name);
        const existingNode = currentLevel.find((node) => node.order === order && node.name === normalizedName);
        const node: NavItem = existingNode ?? {
          name: normalizedName,
          order,
          children: [],
        };
        node.children ??= [];
        nodeMap.set(folderPath, node);
        if (!existingNode) {
          currentLevel.push(node);
        }
      }

      currentLevel = nodeMap.get(folderPath)?.children ?? [];
      currentPath = folderPath;
    }

    // Handle index.md as folder content
    if (isIndex) {
      if (currentPath && nodeMap.has(currentPath)) {
        nodeMap.get(currentPath)!.contentPath = filePath.slice(0, -'/index.md'.length);
      }
      continue;
    }

    // Add file as leaf node
    const { order, name } = parseOrderedName(fileBaseName);
    const folderPath = currentPath ? `${currentPath}/${fileBaseName}` : fileBaseName;
    const existingFolder = nodeMap.get(folderPath);
    if (existingFolder) {
      existingFolder.contentPath = filePath;
      continue;
    }

    const leafNode: NavItem = {
      name: normalizeName(name),
      contentPath: filePath,
      order,
    };
    currentLevel.push(leafNode);
  }

  // Sort all levels by order
  const sortNodes = (nodes: NavItem[]) => {
    nodes.sort((a, b) => a.order - b.order || sortLabel(a).localeCompare(sortLabel(b)));
    for (const node of nodes) {
      if (node.children) {
        sortNodes(node.children);
      }
    }
  };
  sortNodes(root);

  _navTreeCache = root;
  _navTreeCacheExpiresAt = Date.now() + NAV_CACHE_TTL_MS;
  return _navTreeCache;
}

function sortLabel(item: NavItem): string {
  return item.name === FIRST_PAGE_NAME ? '' : item.name;
}

/**
 * Build a map from URL path to contentPath for quick lookup.
 * URL path segments don't have order prefixes, contentPath does.
 * e.g., "getting-started/introduction" -> "01-getting-started/02-introduction.md"
 */
let _pathMapCache: Map<string, string> | undefined;
let _pathMapCacheExpiresAt = 0;

async function buildPathMap(): Promise<Map<string, string>> {
  if (_pathMapCache && Date.now() < _pathMapCacheExpiresAt) {
    return _pathMapCache;
  }

  const files = scanDocsMarkdown();
  const pathMap = new Map<string, string>();

  for (const filePath of files) {
    const parts = filePath.split('/');

    // Skip hidden folders
    if (parts.some((part) => part.startsWith('.'))) {
      continue;
    }

    // Convert contentPath to URL path by stripping order prefixes
    const urlPath = parts
      .map((part) => {
        const parsed = parseOrderedName(part.replace(/\.md$/, ''));
        return parsed.name;
      })
      .join('/');

    // Handle index.md -> folder path
    if (urlPath.endsWith('/index')) {
      pathMap.set(urlPath.replace(/\/index$/, ''), filePath);
    } else {
      pathMap.set(urlPath, filePath);
    }
  }

  _pathMapCache = pathMap;
  _pathMapCacheExpiresAt = Date.now() + NAV_CACHE_TTL_MS;
  return pathMap;
}

/**
 * Resolve a URL path to a contentPath.
 * @param urlPath - URL path without /docs/ prefix, e.g., "getting-started/introduction"
 * @returns contentPath or undefined if not found
 */
export async function resolveContentPath(urlPath: string): Promise<string | undefined> {
  const pathMap = await buildPathMap();
  return pathMap.get(urlPath);
}

/**
 * Enumerate every docs URL splat for SSG/ISR pre-rendering: the keys of
 * {@link buildPathMap}, which are exactly the values {@link resolveContentPath}
 * resolves. Using this as the `page().static({ paths })` source guarantees the
 * pre-rendered paths and the runtime loader lookup can never drift.
 *
 * Server-only (Bun `Glob` + filesystem): import it lazily from the docs page so
 * it is never pulled into the client bundle.
 */
export async function getDocsPaths(): Promise<string[]> {
  const pathMap = await buildPathMap();
  return [...pathMap.keys()].filter((key) => key.length > 0).sort();
}

/**
 * The BAKED docs root: the build-time output the deployed image always
 * carries. This is the fallback the runtime content overlay degrades to.
 */
export function getBakedDocsRoot(): string {
  return joinPath(getProjectRoot(), '.gen', 'public', 'docs');
}

/**
 * Get the docs root directory path every docs-content reader must resolve
 * through: the runtime overlay's active version when one is materialized,
 * else the baked root. Generate-time code also lands here safely —
 * a process that never ingested an overlay always resolves the baked root.
 */
export function getDocsRoot(): string {
  return getActiveDocsRoot(getBakedDocsRoot());
}

/**
 * Drop the nav-tree and path-map caches so the next access re-scans the
 * (possibly overlaid) docs root. Called by the overlay ingest after an active
 * pointer swap; without it, stale nav would keep serving for up to
 * {@link NAV_CACHE_TTL_MS} after a content update.
 */
export function invalidateNavCache(): void {
  _navTreeCacheExpiresAt = 0;
  _pathMapCacheExpiresAt = 0;
}
