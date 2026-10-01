import type { ContainerDescription, DescribeOptions, DiDebugLogger, ProviderDescription } from './container.types';
import { validateProviders } from './container-validation';
import { CircularDependencyError, ContainerClosedError, DuplicateProviderError, NotRegisteredError } from './errors';
import type {
  FilterOptions,
  Provider,
  Registration,
  ResolveFn,
  Token,
  ValidationIssue,
  ValidationResult,
} from './inject.type';
import { useLogger } from '../logger';
import { createScopeProxy } from './proxy';
import { useContainer } from './scope';
import { tokenName } from './token';

export type { ContainerDescription, DescribeOptions, DiDebugLogger, ProviderDescription };

/**
 * Hierarchical DI container with scope awareness, visibility enforcement,
 * circular dependency detection, and lifecycle management.
 *
 * Containers form a tree. Resolution walks up the parent chain.
 * Private providers are only visible within the container that registered them.
 */
export class Container {
  readonly name: string;

  private parent?: Container;
  private children: Container[] = [];
  private providers = new Map<Token, Provider>();
  private instances = new Map<Token, unknown>();
  /** In-flight async resolutions, keyed by token, for concurrent de-duplication. */
  private pendingAsync = new Map<Token, Promise<unknown>>();
  private tags = new Map<string, Set<Token>>();
  private resolving = new Set<Token>();
  private closed = false;
  private closeHooks: Array<{ token: Token; hook: (instance: unknown) => void | Promise<void> }> = [];
  /** @internal */
  debugLogger?: DiDebugLogger;

  constructor(name: string, parent?: Container) {
    this.name = name;
    this.parent = parent;
    if (parent) {
      parent.children.push(this);
      this.debugLogger = parent.debugLogger;
    }
  }

  /**
   * Registers a provider in this container.
   * @throws {DuplicateProviderError} if the token is already registered in this container
   */
  register<T>(registration: Registration<T>): void {
    const { provider } = registration;
    if (this.providers.has(provider.token)) {
      throw new DuplicateProviderError(provider.token, this.name);
    }
    this.providers.set(provider.token, provider as Provider);

    for (const tag of provider.tags) {
      if (!this.tags.has(tag)) {
        this.tags.set(tag, new Set());
      }
      this.tags.get(tag)?.add(provider.token);
    }
  }

  /**
   * Replaces an existing provider registration.
   * If the token is not already registered, behaves like `register()`.
   *
   * Used by `ContainerContextFork` for test overrides.
   * @internal
   */
  replaceProvider<T>(registration: Registration<T>): void {
    const { provider } = registration;

    // Remove old tags if replacing
    const old = this.providers.get(provider.token);
    if (old) {
      for (const tag of old.tags) {
        this.tags.get(tag)?.delete(provider.token);
      }
    }

    this.providers.set(provider.token, provider as Provider);

    for (const tag of provider.tags) {
      if (!this.tags.has(tag)) {
        this.tags.set(tag, new Set());
      }
      this.tags.get(tag)?.add(provider.token);
    }
  }

  /**
   * Resolves an instance from this container or its parent chain.
   *
   * @throws {ContainerClosedError} if the container is closed
   * @throws {NotRegisteredError} if the token is not found
   * @throws {CircularDependencyError} if a circular dependency is detected
   */
  get<T>(token: Token<T>): T {
    return this.resolveToken(token, false);
  }

  /**
   * Internal resolution with visibility control.
   *
   * @param token - The token to resolve
   * @param fromChild - Whether this call originates from a child container
   * @param originChain - The in-flight resolution chain of the container where
   *   resolution originated. Threaded down the parent walk so a cross-container
   *   miss reports the originating dependent(s) rather than the (empty) root
   *   `resolving` set. Defaults to this container's own chain for top-level `get()`.
   */
  private resolveToken<T>(token: Token<T>, fromChild: boolean, originChain?: Token[]): T {
    if (this.closed) {
      throw new ContainerClosedError(this.name);
    }

    // Check cached instances
    if (this.instances.has(token)) {
      const provider = this.providers.get(token);
      if (provider && provider.visibility === 'private' && fromChild) {
        // Private: skip, let parent chain continue
      } else {
        this.debugLogger?.onResolve(token, this.name, true);
        return this.instances.get(token) as T;
      }
    }

    // Check local provider (lazy singletons instantiate on first access)
    const provider = this.providers.get(token);
    if (provider) {
      if (provider.visibility === 'private' && fromChild) {
        // Private: not visible to children, fall through to parent
      } else {
        return this.instantiate(provider) as T;
      }
    }

    // Capture the originating container's in-flight chain on the first hop so a
    // cross-container/dynamic miss (where the root's `resolving` set is empty)
    // still reports the dependent provider that triggered this resolution.
    const chain = originChain ?? [...this.resolving];

    // Walk up to parent, threading the originating chain down the parent walk.
    if (this.parent) {
      return this.parent.resolveToken(token, true, chain);
    }

    throw new NotRegisteredError(token, this.name, chain);
  }

  /**
   * Instantiates a provider, resolving its dependencies.
   */
  private instantiate<T>(provider: Provider<T>): T {
    // Already resolved (race condition guard for sync)
    if (this.instances.has(provider.token)) {
      return this.instances.get(provider.token) as T;
    }

    // Circular dependency detection
    if (this.resolving.has(provider.token)) {
      const chain = [...this.resolving, provider.token];
      throw new CircularDependencyError(chain);
    }

    this.resolving.add(provider.token);
    try {
      const resolve = this.createResolveFn(provider);

      let instance: T;
      if (provider.async) {
        // Async factories are resolved up front by resolveAll() during start(),
        // which caches the instance before any sync dependent is instantiated.
        // Reaching here means the instance was never resolved — e.g. the provider
        // is marked `lazy` (deferred async is unsupported) or this is being
        // resolved outside the start() lifecycle.
        throw new Error(
          `Async factory for ${tokenName(provider.token)} was not resolved during start(). ` +
            'Async factories must be eagerly resolved at startup; they cannot be marked `lazy` ' +
            'nor resolved synchronously after start().',
        );
      }

      instance = (provider.factory as (resolve: ResolveFn) => T)(resolve);

      // Guard against a sync factory that returns a Promise without being
      // declared `async`. `isAsyncFactory()` keys on the `async` keyword, so a
      // plain arrow that forwards a Promise (e.g. `(resolve) => connect(...)`)
      // is classified sync. Caching the pending Promise as the instance would
      // silently hand `get(token)` an unresolved Promise of the wrong type.
      // Fail fast with guidance instead of auto-awaiting.
      if (isThenable(instance)) {
        throw new Error(
          `Sync factory for ${tokenName(provider.token)} returned a Promise, but the factory is not declared async. ` +
            'Declare the factory with the `async` keyword (e.g. `provide(Token, async (resolve) => ...)`) so it is ' +
            'eagerly awaited during start(); otherwise the pending Promise would be cached as the instance.',
        );
      }

      this.instances.set(provider.token, instance);
      this.debugLogger?.onResolve(provider.token, this.name, false);

      if (provider.onClose) {
        this.closeHooks.push({
          token: provider.token,
          hook: provider.onClose as (instance: unknown) => void | Promise<void>,
        });
      }

      return instance;
    } finally {
      this.resolving.delete(provider.token);
    }
  }

  /**
   * Resolves an async provider. Only called during `start()`.
   *
   * Concurrent calls for the same token share a single in-flight Promise so the
   * async factory runs exactly once. Without this de-duplication two callers
   * that both miss the instance cache (e.g. `resolveAsync` racing `refresh()`)
   * would each run the factory and leak the duplicate (DB pools, clients, ...).
   */
  async resolveAsync<T>(provider: Provider<T>): Promise<T> {
    if (this.instances.has(provider.token)) {
      return this.instances.get(provider.token) as T;
    }

    // De-duplicate concurrent resolutions: return the existing in-flight Promise.
    const pending = this.pendingAsync.get(provider.token);
    if (pending) {
      return pending as Promise<T>;
    }

    const promise = this.runAsyncFactory(provider).finally(() => {
      this.pendingAsync.delete(provider.token);
    });
    this.pendingAsync.set(provider.token, promise);
    return promise;
  }

  /**
   * Runs an async provider's factory once and caches the resulting instance.
   * Callers must go through {@link resolveAsync} so concurrent resolutions are
   * de-duplicated via the in-flight Promise cache.
   */
  private async runAsyncFactory<T>(provider: Provider<T>): Promise<T> {
    this.resolving.add(provider.token);
    try {
      const resolve: ResolveFn = Object.assign(<D>(dep: Token<D>): D => this.get(dep), {
        all: <D>(filter: FilterOptions): D[] => this.list(filter),
      });

      const instance = await (provider.factory as (resolve: ResolveFn) => Promise<T>)(resolve);
      this.instances.set(provider.token, instance);

      if (provider.onClose) {
        this.closeHooks.push({
          token: provider.token,
          hook: provider.onClose as (instance: unknown) => void | Promise<void>,
        });
      }

      return instance;
    } finally {
      this.resolving.delete(provider.token);
    }
  }

  /**
   * Lists all instances matching a filter across the container hierarchy.
   *
   * @param filter - Filter options (currently supports tags)
   * @returns Array of matching instances
   *
   * @example
   * ```typescript
   * const plugins = container.list<Plugin>({ tags: 'plugin' });
   * const secure = container.list<Middleware>({ tags: ['http', 'auth'] });
   * ```
   */
  list<T = unknown>(filter: FilterOptions): T[] {
    if (this.closed) {
      throw new ContainerClosedError(this.name);
    }

    const results: T[] = [];
    const filterTags = normalizeFilterTags(filter.tags);
    this.collectFiltered(filterTags, results, false);
    return results;
  }

  /**
   * Collects all instances matching filter tags from this container and parents.
   */
  private collectFiltered<T>(filterTags: string[], results: T[], fromChild: boolean): void {
    if (filterTags.length === 0) return;

    // Find tokens that match ALL filter tags
    const matchingTokens = new Set<Token>();

    // Start with the first tag's tokens as candidates
    const firstTagTokens = this.tags.get(filterTags[0]);
    if (firstTagTokens) {
      for (const token of firstTagTokens) {
        const provider = this.providers.get(token);
        if (provider && provider.visibility === 'private' && fromChild) {
          continue;
        }
        // Check the candidate matches ALL remaining tags using the tag index (O(1) per lookup)
        let matchesAll = true;
        for (let i = 1; i < filterTags.length; i++) {
          if (!this.tags.get(filterTags[i])?.has(token)) {
            matchesAll = false;
            break;
          }
        }
        if (matchesAll) {
          matchingTokens.add(token);
        }
      }
    }

    for (const token of matchingTokens) {
      results.push(this.get(token) as T);
    }

    if (this.parent) {
      this.parent.collectFiltered(filterTags, results, true);
    }
  }

  /**
   * Checks whether this container itself exposes the token to an OUTSIDE
   * caller: the provider must be registered here and must not be private.
   * The parent chain is deliberately not consulted — the outside caller
   * (the context walking mounted modules) has already searched the root,
   * and a private provider resolves only inside its declaring container.
   */
  hasPublic(token: Token): boolean {
    const provider = this.providers.get(token);
    return provider !== undefined && provider.visibility !== 'private';
  }

  /**
   * Finds the provider an OUTSIDE caller may resolve from this container:
   * local and not private, with no parent walk.
   */
  findPublicProvider(token: Token): Provider | undefined {
    const provider = this.providers.get(token);
    if (provider && provider.visibility !== 'private') {
      return provider;
    }
    return undefined;
  }

  /**
   * Like {@link list}, but as seen by an OUTSIDE caller: this container's
   * private providers are skipped, exactly as they are for a child.
   */
  listPublic<T = unknown>(filter: FilterOptions): T[] {
    if (this.closed) {
      throw new ContainerClosedError(this.name);
    }
    const results: T[] = [];
    this.collectFiltered(normalizeFilterTags(filter.tags), results, true);
    return results;
  }

  /**
   * Checks if a token is available in this container or its parents.
   */
  has(token: Token): boolean {
    return this.hasToken(token, false);
  }

  private hasToken(token: Token, fromChild: boolean): boolean {
    const provider = this.providers.get(token);
    if (provider) {
      if (provider.visibility === 'private' && fromChild) {
        // Private: not visible to children
      } else {
        return true;
      }
    }
    if (this.parent) {
      return this.parent.hasToken(token, true);
    }
    return false;
  }

  /**
   * Creates a child container.
   */
  createChild(name: string): Container {
    return new Container(name, this);
  }

  /**
   * Finds a provider in this container or its parent chain.
   */
  findProvider(token: Token): Provider | undefined {
    const provider = this.providers.get(token);
    if (provider) {
      return provider;
    }
    if (this.parent) {
      return this.parent.findProvider(token);
    }
    return undefined;
  }

  /**
   * Finds the provider that would be visible to resolution from this container.
   *
   * Unlike {@link findProvider}, this respects `private` visibility when walking
   * from a child to a parent. It does not instantiate the provider.
   */
  findVisibleProvider(token: Token): Provider | undefined {
    return this.findVisibleProviderFrom(token, false);
  }

  private findVisibleProviderFrom(token: Token, fromChild: boolean): Provider | undefined {
    const provider = this.providers.get(token);
    if (provider && !(provider.visibility === 'private' && fromChild)) {
      return provider;
    }
    if (this.parent) {
      return this.parent.findVisibleProviderFrom(token, true);
    }
    return undefined;
  }

  /**
   * Returns the cached instance for a token, or undefined.
   * Used for post-instantiation wrapping (e.g. tracing).
   */
  getInstance(token: Token): unknown {
    return this.instances.get(token);
  }

  /**
   * Replaces the cached instance for a token.
   * Used for post-instantiation wrapping (e.g. tracing).
   */
  setInstance(token: Token, instance: unknown): void {
    this.instances.set(token, instance);
  }

  /**
   * Validates all providers in this container and its children.
   * Checks for: missing deps, circular deps, scope violations, duplicates.
   *
   * Returns a structured result with all issues found. Each issue includes
   * the container name and tokens involved for debugging.
   */
  validate(): ValidationResult {
    const issues = this.validateRecursive();
    return { valid: issues.length === 0, issues };
  }

  private validateRecursive(): ValidationIssue[] {
    const issues = validateProviders(
      this.providers,
      this.name,
      (token, fromChild) => this.hasToken(token, fromChild),
      (token) => this.findProvider(token),
    );
    for (const child of this.children) {
      issues.push(...child.validateRecursive());
    }
    return issues;
  }

  /**
   * Resolves all singleton providers (including async factories).
   * Called during container context startup after validation.
   *
   * Providers are resolved in **dependency order**, not registration order:
   * a provider's same-container singleton dependencies are resolved before the
   * provider itself. This lets a sync singleton depend on an async-initialized
   * singleton regardless of the order they were registered in — the async
   * dependency is awaited and cached before the sync dependent's factory runs.
   *
   * Skips providers marked with `{ lazy: true }` during this dependency-order
   * traversal. If an eager provider's factory resolves a lazy dependency, normal
   * resolution still instantiates that dependency.
   *
   * Cycles (which `validate()` already rejects before `start()`) are guarded
   * against here too: a provider already being resolved is skipped rather than
   * re-entered, so resolution never loops forever.
   */
  async resolveAll(): Promise<void> {
    const inProgress = new Set<Token>();
    for (const [token] of this.providers) {
      await this.resolveEager(token, inProgress);
    }

    // Resolve children
    for (const child of this.children) {
      await child.resolveAll();
    }
  }

  /**
   * Eagerly resolves a single non-lazy singleton, resolving its same-container
   * singleton dependencies first (depth-first / topological order).
   *
   * @param token - The provider token to resolve eagerly.
   * @param inProgress - Tokens currently on the resolution stack, used to break
   *   dependency cycles so the traversal cannot recurse forever.
   */
  private async resolveEager(token: Token, inProgress: Set<Token>): Promise<void> {
    const provider = this.providers.get(token);
    if (!provider) return; // Cross-container dep — resolved lazily via the parent chain.
    if (provider.scope !== 'singleton') return;
    if (provider.lazy) return;
    if (this.instances.has(token)) return;
    if (inProgress.has(token)) return; // Cycle guard: leave it to instantiate()'s detection.

    inProgress.add(token);
    try {
      // Resolve same-container singleton dependencies first so they are cached
      // before this provider's factory runs.
      for (const dep of provider.deps) {
        await this.resolveEager(dep, inProgress);
      }

      if (this.instances.has(token)) return; // A dep cycle may have resolved it.

      if (provider.async) {
        await this.resolveAsync(provider);
      } else {
        this.instantiate(provider);
      }
    } finally {
      inProgress.delete(token);
    }
  }

  /**
   * Refreshes all dynamic providers by clearing their cached instances
   * and re-executing their factories. Non-dynamic providers are untouched.
   *
   * Note: only the dynamic provider's own instance is replaced. Singletons
   * that already hold a reference to the old instance are not invalidated.
   */
  async refreshDynamic(): Promise<void> {
    for (const [, provider] of this.providers) {
      if (!provider.dynamic) continue;
      if (provider.scope !== 'singleton') continue;

      // Run onClose for the old instance if it exists
      const oldInstance = this.instances.get(provider.token);
      if (oldInstance !== undefined && provider.onClose) {
        await provider.onClose(oldInstance as never);
      }

      // Clear cached instance so it gets re-created
      this.instances.delete(provider.token);

      // Re-instantiate
      if (provider.async) {
        await this.resolveAsync(provider);
      } else {
        this.instantiate(provider);
      }
    }

    // Recurse into children
    for (const child of this.children) {
      await child.refreshDynamic();
    }
  }

  /**
   * Closes this container and all children, calling onClose hooks in reverse order.
   */
  async close(): Promise<void> {
    const logger = useLogger('putnami:di');

    // Close children first (reverse order). A single child's failure must not
    // abort closing the remaining children.
    for (let i = this.children.length - 1; i >= 0; i--) {
      try {
        await this.children[i].close();
      } catch (err) {
        logger.error(`[close] child container '${this.children[i].name}' failed to close`, err);
      }
    }

    // Run close hooks in reverse registration order. Each hook is isolated so a
    // single throwing/rejecting hook can't strand earlier-registered cleanup
    // (DB pools, connections) or leave the container half-open.
    for (let i = this.closeHooks.length - 1; i >= 0; i--) {
      const { token, hook } = this.closeHooks[i];
      const instance = this.instances.get(token);
      if (instance !== undefined) {
        try {
          await hook(instance);
        } catch (err) {
          logger.error(`[close] onClose hook for '${tokenName(token)}' failed`, err);
        }
      }
    }

    this.closed = true;
    this.instances.clear();
    this.pendingAsync.clear();
    this.closeHooks = [];
  }

  /**
   * Gets all providers in this container (for introspection/context building).
   */
  getProviders(): Map<Token, Provider> {
    return this.providers;
  }

  /**
   * Returns a debug-friendly summary of this container's state.
   * Useful for diagnosing DI issues.
   *
   * @param options - Pass `{ validate: true }` to include validation issues inline.
   */
  describe(options?: DescribeOptions): ContainerDescription {
    const providers: ProviderDescription[] = [];
    for (const [, provider] of this.providers) {
      providers.push({
        token: tokenName(provider.token),
        scope: provider.scope,
        visibility: provider.visibility,
        async: provider.async,
        lazy: provider.lazy ?? false,
        dynamic: provider.dynamic ?? false,
        tags: provider.tags,
        deps: provider.deps.map(tokenName),
        resolved: this.instances.has(provider.token),
      });
    }

    const description: ContainerDescription = {
      name: this.name,
      closed: this.closed,
      providers,
      children: this.children.map((c) => c.describe(options)),
    };

    if (options?.validate) {
      const result = this.validate();
      description.issues = result.issues;
    }

    return description;
  }

  /**
   * Creates a ResolveFn for a provider that handles scope proxies and tag resolution.
   */
  private createResolveFn<T>(provider: Provider<T>): ResolveFn {
    const resolveSingle = <D>(dep: Token<D>): D => {
      // Check if the dependency is scoped and we're in a singleton context
      const depProvider = this.findProvider(dep);
      if (depProvider && depProvider.scope === 'scoped' && provider.scope === 'singleton') {
        // Log scope proxy creation (once per token pair via the logger's dedup)
        this.debugLogger?.onScopeProxy(provider.token, dep);
        // Return a scope proxy that resolves from the current async scope.
        // At access time, useContainer() reads from AsyncLocalStorage (set by ContainerContext.scope()).
        return createScopeProxy(dep, () => useContainer()) as D;
      }
      return this.get(dep);
    };
    return Object.assign(resolveSingle, {
      all: <D>(filter: FilterOptions): D[] => this.list(filter),
    });
  }
}

/**
 * A scoped container that resolves scoped providers locally
 * and delegates singletons to its parent.
 */
export class ScopedContainer extends Container {}

/**
 * Detects a thenable (Promise-like) value: non-null with a callable `then`.
 * Used to catch sync factories that return a Promise without being declared
 * `async`, which would otherwise cache the pending Promise as the instance.
 */
function isThenable(value: unknown): value is PromiseLike<unknown> {
  return (
    value != null &&
    (typeof value === 'object' || typeof value === 'function') &&
    typeof (value as { then?: unknown }).then === 'function'
  );
}

/**
 * Normalizes the `tags` filter option into a string array.
 */
function normalizeFilterTags(tags: string | string[] | undefined): string[] {
  if (!tags) return [];
  return Array.isArray(tags) ? tags : [tags];
}
