import type { AnalyticsRuntime } from '../runtime';

/**
 * What the server tells the browser about this page view.
 *
 * The list is exhaustive and deliberately short: it carries no `user_id`, no
 * visitor id, no secret, and no configuration beyond the four envelope fields
 * a client event has to repeat. A browser that reads this object learns
 * nothing about the visitor it does not already know about itself.
 */
export interface AnalyticsBootstrap {
  /** The server-minted page-view event id the client re-sends enrichment for. */
  pv: string;
  /** The matched route pattern of this view. */
  route: string;
  /** Where the tracker POSTs its batches. */
  endpoint: string;
  /** The application name. */
  app: string;
  /** The deployment environment. */
  env: string;
  /** The application version, or null. */
  version: string | null;
  /** The action names the server accepts; anything else is dropped on arrival. */
  declared: string[];
}

/**
 * Builds the bootstrap object the page renderer serializes as
 * `window.__putnamiBootstrap.analytics`.
 *
 * The event id is the server's: the client re-sends the *same* id with its
 * engagement, and the raw upsert takes the conflict branch, so a page view is
 * counted once whether the tracker ever loads or not.
 *
 * @param rt - The analytics runtime.
 * @param pv - The server-minted page-view event id.
 * @param route - The matched route pattern, or `__unmatched__`.
 * @returns The bootstrap payload.
 */
export function buildBootstrap(rt: AnalyticsRuntime, pv: string, route: string): AnalyticsBootstrap {
  return {
    pv,
    route,
    endpoint: rt.endpoint,
    app: rt.app,
    env: rt.env,
    version: rt.version,
    // Sorted so two renders of the same page produce byte-identical HTML.
    declared: [...rt.declared].sort(),
  };
}
