import type { Provider, Token } from './inject.type';

/**
 * Static (no-execution) analysis of the DI dependency graph.
 *
 * The container records each provider's declared dependencies (`Provider.deps`)
 * and lifecycle scope (`Provider.scope`). That makes a provider's transitive
 * dependency set — and therefore whether it can reach a request/session-scoped
 * provider — a property of the graph that can be computed **before** anything is
 * instantiated. The web framework uses this to prove a `.static()` route never
 * depends on request-scoped state.
 *
 * The proof is sound but **conservative**: a factory provider whose `deps` are
 * not known to be complete (`Provider.depsComplete === false`) is treated as
 * opaque, because its factory body may resolve undeclared tokens. Opaque edges
 * make the result a *hybrid* — not proven dynamic, but not provably static
 * either — so a runtime guard must remain the backstop.
 */

/** A reachable request/session-scoped provider, with the path that reaches it. */
export interface ScopeReachabilityHit {
  /** The reachable `scoped` provider token. */
  readonly token: Token;
  /** Resolution path from a root token to this provider (root → … → token). */
  readonly path: readonly Token[];
}

/** Result of {@link analyzeScopeReachability}. */
export interface ScopeReachability {
  /**
   * Reachable providers whose scope is `scoped` — each one is a hard
   * static-safety violation (a static graph must not depend on request scope).
   */
  readonly scoped: readonly ScopeReachabilityHit[];
  /**
   * Tokens whose subtree could not be fully analyzed: a factory provider without
   * `depsComplete`, or a token with no registered provider (resolved from a
   * parent container or dynamically at runtime). Their presence means the proof
   * is a hybrid that a runtime guard must back up.
   */
  readonly opaque: readonly Token[];
  /** True when every reachable provider had a fully-enumerable dependency set. */
  readonly decidable: boolean;
}

/**
 * Context for provider lookup while walking dependencies.
 *
 * `from` is the provider whose declared `deps` are currently being traversed.
 * Container-aware lookups use this to mirror real DI resolution: a provider
 * registered in a module resolves dependencies from that module first, then its
 * parent chain.
 */
export interface ProviderLookupContext {
  readonly from?: Provider;
}

/** Resolves a token to its provider descriptor, or `undefined` if unregistered. */
export type ProviderLookup = (token: Token, context?: ProviderLookupContext) => Provider | undefined;

/**
 * Walk the DI dependency graph from a set of root tokens and report which
 * request/session-scoped providers are transitively reachable, and whether the
 * walk was fully decidable.
 *
 * Pure and side-effect free: it never instantiates a provider. Cycles are
 * handled by visiting each provider at most once.
 *
 * @param roots - The entry tokens (e.g. the tokens a route's loader injects).
 * @param lookup - Resolves a token to its `Provider` (or `undefined`).
 */
export function analyzeScopeReachability(roots: readonly Token[], lookup: ProviderLookup): ScopeReachability {
  const scoped: ScopeReachabilityHit[] = [];
  const opaque: Token[] = [];
  const seenScoped = new Set<Token>();
  const seenOpaque = new Set<Token>();
  const visitedProviders = new Set<Provider>();
  const visitedMissing = new Set<Token>();

  const markOpaque = (token: Token): void => {
    if (!seenOpaque.has(token)) {
      seenOpaque.add(token);
      opaque.push(token);
    }
  };

  const visit = (token: Token, path: readonly Token[], from?: Provider): void => {
    const provider = lookup(token, from ? { from } : undefined);
    if (!provider) {
      // No registered provider for this token — it resolves from a parent
      // container or dynamically. Its scope is unknown, so the subtree is opaque.
      if (visitedMissing.has(token)) return;
      visitedMissing.add(token);
      markOpaque(token);
      return;
    }
    if (visitedProviders.has(provider)) return;
    visitedProviders.add(provider);

    const nextPath = [...path, token];

    if (provider.scope === 'scoped' && !seenScoped.has(token)) {
      seenScoped.add(token);
      scoped.push({ token, path: nextPath });
    }

    if (!provider.depsComplete) {
      // The factory may resolve tokens beyond `deps`; we can analyze the declared
      // deps below, but cannot prove the subtree is complete.
      markOpaque(token);
    }

    for (const dep of provider.deps) {
      visit(dep, nextPath, provider);
    }
  };

  for (const root of roots) {
    visit(root, []);
  }

  return { scoped, opaque, decidable: opaque.length === 0 };
}
