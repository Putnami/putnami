import { analyzeScopeReachability, type ProviderLookup, type Token, tokenName } from '@putnami/runtime/inject';
import { StaticRenderViolation } from './static';

/**
 * DI-graph static-safety proof for `.static()` routes.
 *
 * The runtime guard (`createStaticRenderContext`) only fires if the offending
 * code path *happens to run* during pre-render. This promotes the guarantee to
 * a **static proof from the DI dependency graph**: because request/session-scoped
 * dependencies resolve through the container, the framework can decide — before
 * rendering, independent of runtime branches — whether a route is static-safe.
 */

/** The DI roots a single `.static()` route resolves, plus its identity. */
export interface StaticRouteRoots {
  /** React-Router style route, e.g. `/blog/:slug`. */
  readonly route: string;
  /** Concrete provider tokens the route's loader injects. */
  readonly roots: readonly Token[];
  /**
   * The loader injects a tag/filter selector, so its roots resolve to a
   * runtime-determined set of providers and can't be fully enumerated. Such a
   * route is a hybrid even when no scoped provider is found.
   */
  readonly dynamicRoots?: boolean;
}

/** Outcome of {@link proveStaticRouteSafety} for one route. */
export interface StaticSafetyProof {
  readonly route: string;
  /**
   * `true` only when the DI graph **proves** the route is request-independent:
   * every reachable provider is a singleton with a fully-enumerable dependency
   * set, and the loader injects no dynamic selectors.
   *
   * `false` marks a **hybrid** — not proven dynamic (no scoped provider was
   * reached), but an opaque factory or dynamic selector means the runtime
   * `StaticRenderViolation` guard remains the backstop.
   */
  readonly diProven: boolean;
}

/**
 * Prove, from the DI dependency graph, that a `.static()` route does not depend
 * on request/session-scoped state.
 *
 * Throws {@link StaticRenderViolation} (via `StaticRenderViolation.scope`) when
 * the graph reaches a scoped provider — a hard, deterministic build error naming
 * the route, the offending provider and the resolution path.
 *
 * Returns a {@link StaticSafetyProof} otherwise: `diProven` distinguishes a graph
 * that *proves* static-safety from a hybrid that the runtime guard backstops.
 *
 * @param spec - The route and the DI roots its loader injects.
 * @param lookup - Resolves a token to its provider (e.g.
 *   `containerContext.analyzeScopeReachability` is the wired-up equivalent; this
 *   takes the raw lookup so it works at build time too).
 */
export function proveStaticRouteSafety(spec: StaticRouteRoots, lookup: ProviderLookup): StaticSafetyProof {
  const { route, roots, dynamicRoots = false } = spec;
  const reachability = analyzeScopeReachability(roots, lookup);

  if (reachability.scoped.length > 0) {
    const hit = reachability.scoped[0];
    throw StaticRenderViolation.scope(
      route,
      tokenName(hit.token),
      hit.path.map((token) => tokenName(token)),
    );
  }

  return { route, diProven: reachability.decidable && !dynamicRoots };
}
