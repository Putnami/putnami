import type { BaseRouteNode } from '../shared/route.types';

/**
 * Shared utilities for working with route trees.
 * Used by both ReactApplication (server) and ReactClientGenerator (client).
 */

/**
 * Callbacks for node-specific operations.
 * Allows the helper to work with different node types.
 */
interface RouteTreeCallbacks<T extends BaseRouteNode> {
  /** Check if a node is a layout */
  isLayout: (node: T) => boolean;
  /** Create a new layout node with the given path and id */
  createLayoutNode: (path: string, id: string) => T;
}

/**
 * Converts Next.js-style bracket params to React Router params.
 * @example convertBracketToParam('[id]') => ':id'
 * @example convertBracketToParam('[...path]') => '*'
 * @example convertBracketToParam('[[...path]]') => '*'
 */
export function convertBracketToParam(segment: string): string {
  if (segment.startsWith('[') && segment.endsWith(']')) {
    const inner = segment.slice(1, -1);
    // Handle catch-all: [...param] or optional catch-all: [[...param]]
    if (inner.startsWith('...') || (inner.startsWith('[...') && inner.endsWith(']'))) {
      return '*';
    }
    // Handle optional catch-all: [[...param]]
    if (inner.startsWith('[') && inner.endsWith(']') && inner.slice(1, -1).startsWith('...')) {
      return '*';
    }
    return `:${inner}`;
  }
  return segment;
}

/**
 * Parses a route string into segments, filtering empty ones.
 * @example parseRouteSegments('/users/[id]/settings') => ['users', '[id]', 'settings']
 */
function parseRouteSegments(route: string): string[] {
  return route.split('/').filter((s) => s.length > 0);
}

/**
 * Filters out route group segments (parentheses notation).
 * @example filterRouteGroups(['(auth)', 'login']) => ['login']
 */
function filterRouteGroups(segments: string[]): string[] {
  return segments.filter((s) => !s.startsWith('(') || !s.endsWith(')'));
}

/**
 * Builds an ID path from segments, cleaning special characters.
 * @example buildIdPath('root', ['users', '[id]']) => 'root-users-id'
 */
function buildIdPath(base: string, segments: string[]): string {
  let idPath = base;
  for (const segment of segments) {
    idPath += `-${segment.replace(/[:[\]]/g, '')}`;
  }
  return idPath;
}

/**
 * Generic helper for working with route trees.
 * Provides common operations for both server and client routing.
 */
export class RouteTreeHelper<T extends BaseRouteNode> {
  constructor(
    private readonly root: T,
    private readonly callbacks: RouteTreeCallbacks<T>,
  ) {}

  /**
   * Find or create a route node for layouts only.
   * Creates the hierarchical structure for layouts.
   */
  ensureLayoutRoute(route: string): T {
    const segments = parseRouteSegments(route);
    const routeSegments = filterRouteGroups(segments);

    let current = this.root;
    let idPath = 'root';

    for (const segment of routeSegments) {
      const converted = convertBracketToParam(segment);
      idPath = buildIdPath(idPath, [segment]);

      let found: T | undefined;
      for (const child of (current.children || []) as T[]) {
        if (child.path === converted) {
          found = child;
          break;
        }
      }

      if (!found) {
        found = this.callbacks.createLayoutNode(converted, idPath);
        current.children ||= [];
        (current.children as T[]).push(found);
      }

      current = found;
    }

    return current;
  }

  private layoutCache = new Map<string, { layout: T; remainingPath: string }>();

  /**
   * Find the nearest parent layout for a given route.
   * Returns the layout node and the remaining path segments (converted to React Router format).
   */
  findNearestLayout(route: string): { layout: T; remainingPath: string } {
    const cached = this.layoutCache.get(route);
    if (cached) {
      return cached;
    }

    const segments = parseRouteSegments(route);
    const routeSegments = filterRouteGroups(segments);

    let currentLayout = this.root;
    let consumedSegments = 0;

    for (let i = 0; i < routeSegments.length; i++) {
      const segment = routeSegments[i];
      const converted = convertBracketToParam(segment);

      let found: T | undefined;
      for (const child of (currentLayout.children || []) as T[]) {
        if (child.path === converted && this.callbacks.isLayout(child)) {
          found = child;
          break;
        }
      }

      if (found) {
        currentLayout = found;
        consumedSegments = i + 1;
      }
    }

    const remainingSegments = routeSegments.slice(consumedSegments);
    const remainingPath = remainingSegments.map(convertBracketToParam).join('/');

    const result = { layout: currentLayout, remainingPath };
    this.layoutCache.set(route, result);
    return result;
  }
}

/**
 * Generates a consistent page ID based on the layout ID and remaining path.
 * Used by both ReactApplication (server) and ReactClientGenerator (client).
 *
 * @param layoutId - The ID of the parent layout (e.g. 'root-layout' or 'root')
 * @param remainingPath - The path remaining after the layout (e.g. 'users/:id')
 */
export function generatePageId(layoutId: string, remainingPath: string): string {
  const pageIdBase = layoutId.replace(/-layout$/, '');

  const pathForId = remainingPath
    .replace(/[/:[\]]/g, '-')
    .replace(/-+/g, '-')
    .replace(/^-|-$/g, '');

  return pathForId ? `${pageIdBase}-${pathForId}-page` : `${pageIdBase}-page`;
}
