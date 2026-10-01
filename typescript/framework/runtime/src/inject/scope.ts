import { tryContext } from '../context';
import { NotRegisteredError } from './errors';
import type { FilterOptions, ResolvedMap, ScopeContext, TagSelector, Token } from './inject.type';
import { isFilterOptions, isTagSelector, tokenName } from './token';

/**
 * Shared symbol for storing the scoped container in async context.
 * Uses `Symbol.for` so it's identical across module instances.
 */
export const SCOPE_CONTAINER_KEY = Symbol.for('__di_scope_container__');

/**
 * Returns the current scoped DI container from async context.
 *
 * @internal This is a low-level escape hatch. No semver guarantees.
 * Prefer `.inject()` on endpoint/loader/action builders for handler-level DI,
 * or `context.get()` / `scope.get()` for app-level resolution.
 *
 * @throws {Error} if called outside of a scope
 */
export function useContainer(): ScopeContext {
  const ctx = tryContext<Record<symbol, unknown>>();
  const scope = ctx?.[SCOPE_CONTAINER_KEY];
  if (!scope) {
    throw new Error(
      'No active DI scope. ' +
        'resolve() / useContainer() can only be called inside a scope. ' +
        'Make sure your handler is wrapped in a scope (the HTTP framework does this automatically).',
    );
  }
  return scope as ScopeContext;
}

/**
 * Resolves a single dependency from the current scoped container.
 *
 * @internal This is a low-level escape hatch. No semver guarantees.
 * Prefer `.inject()` on endpoint/loader/action builders for handler-level DI.
 *
 * @throws {Error} if called outside of a scope
 */
export function resolve<T>(token: Token<T>): T {
  return useContainer().get(token);
}

/**
 * Resolves a map of tokens from a scope context.
 *
 * @internal Used by endpoint/loader/action `.inject()` wrappers.
 * No semver guarantees. Application code should use `.inject()` instead.
 */
export function resolveInjection<M extends Record<string, Token | TagSelector | FilterOptions>>(
  tokens: M,
  scope: ScopeContext,
): ResolvedMap<M> {
  const result: Record<string, unknown> = {};
  for (const [key, token] of Object.entries(tokens)) {
    if (isTagSelector(token)) {
      result[key] = scope.list({ tags: token.tags });
    } else if (isFilterOptions(token)) {
      result[key] = scope.list(token);
    } else {
      try {
        result[key] = scope.get(token as Token);
      } catch (err) {
        // Detect the "not registered" case structurally (instanceof, not by string
        // name) so the actionable enrichment survives renames/subclasses and the
        // rethrown error keeps the NotRegisteredError prototype for downstream
        // instanceof checks.
        if (err instanceof NotRegisteredError) {
          // Re-throw a real NotRegisteredError carrying the same resolution
          // metadata, with the injection key prepended so callers can see which
          // inject() token failed. The original error is preserved via `cause`.
          const enriched = new NotRegisteredError(
            err.token,
            err.containerName,
            err.resolutionChain,
            err.searchedContainers,
          );
          enriched.message = `Failed to resolve inject({ ${key}: ${tokenName(token as Token)} }): ${err.message}`;
          enriched.cause = err;
          throw enriched;
        }
        throw err;
      }
    }
  }
  return result as ResolvedMap<M>;
}
