import type { RouteObject, StaticHandler } from 'react-router';
import { createStaticHandler } from 'react-router';

/**
 * The React Router static handlers of one application, built once per route
 * tree and basename instead of once per request.
 *
 * Building a handler converts the tree to data routes, flattens it, ranks its
 * branches and compiles every path: work that grows with the route count and
 * that a tree which does not change makes once. The application mutates its
 * tree in place while routes register, so it calls {@link invalidate} after
 * each registration; a handler is also rebuilt when the accessor returns
 * another tree than the one it was built from.
 */
export interface StaticHandlerCache {
  /**
   * The handler of `source` for `key` (the locale and the basename), built
   * from the tree `tree` returns when none is cached for that source.
   */
  handler(source: RouteObject[], key: string, tree: () => RouteObject[], basename: string | undefined): StaticHandler;
  /** Drop every handler: the route tree changed. */
  invalidate(): void;
}

export function createStaticHandlerCache(): StaticHandlerCache {
  // One entry per locale and basename: a bounded set for one application.
  const entries = new Map<string, { source: RouteObject[]; handler: StaticHandler }>();
  return {
    handler(source, key, tree, basename) {
      const entry = entries.get(key);
      if (entry && entry.source === source) return entry.handler;
      const handler = createStaticHandler(tree(), basename ? { basename } : undefined);
      entries.set(key, { source, handler });
      return handler;
    },
    invalidate() {
      entries.clear();
    },
  };
}
