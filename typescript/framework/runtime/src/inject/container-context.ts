import { runInContext } from '../context';
import { useLogger } from '../logger';
import {
  Container,
  type ContainerDescription,
  type DescribeOptions,
  type DiDebugLogger,
  ScopedContainer,
} from './container';
import { applyTracing, collectScopedProviders, createDiDebugLogger } from './container-context-helpers';
import { ContainerClosedError, ContainerValidationError, NotRegisteredError, RequirementNotMetError } from './errors';
import type {
  ContainerContextOptions,
  ContainerHolder,
  DetachedScope,
  FilterOptions,
  Provider,
  Registration,
  ScopeContext,
  Token,
} from './inject.type';
import { noopTraceSink } from './proxy';
import { SCOPE_CONTAINER_KEY } from './scope';
import { analyzeScopeReachability, type ProviderLookup, type ScopeReachability } from './static-analysis';

/**
 * A consolidated resolution context over a container tree.
 *
 * ContainerContext is the unified interface for DI resolution across
 * an application and its modules. It manages the full lifecycle:
 * mount → validate → resolve → live → close.
 *
 * Created lazily by Application (in `@putnami/application`) after warmup,
 * or on first `get()` call.
 *
 * @example
 * ```typescript
 * const ctx = new ContainerContext('app');
 * ctx.register(provide(Database, async () => Database.connect(url)));
 * ctx.mount('auth', authModule.getRegistrations(), authModule.getRequirements());
 * await ctx.start();
 * const db = ctx.get(Database);
 * await ctx.close();
 * ```
 */
export class ContainerContext {
  private root: Container;
  private moduleContainers: Container[] = [];
  private scopedProviderCache?: Array<{ provider: Provider }>;
  private state: 'idle' | 'started' | 'closed' = 'idle';
  private options: ContainerContextOptions;
  private diDebugLogger?: DiDebugLogger;

  constructor(name: string, options: ContainerContextOptions = {}) {
    this.options = options;
    this.root = new Container(name);

    // Wire debug logger when enabled (no-op in production)
    this.diDebugLogger = createDiDebugLogger(options.debug);
    if (this.diDebugLogger) {
      this.root.debugLogger = this.diDebugLogger;
    }
  }

  /**
   * Registers a provider at the root level.
   */
  register<T>(registration: Registration<T>): void {
    this.root.register(registration);
  }

  /**
   * Mounts a ContainerHolder (Module) as a child container.
   *
   * Validates that all requirements declared by the holder are available
   * in the root container, then creates a child container with the
   * holder's registrations.
   *
   * @param holder - The ContainerHolder to mount
   * @param name - The name for the child container
   * @returns The child container (for introspection)
   * @throws {RequirementNotMetError} if a required token is not available
   */
  mount(holder: ContainerHolder, name: string): Container {
    // Validate requirements
    for (const token of holder.getRequirements()) {
      if (!this.root.has(token)) {
        throw new RequirementNotMetError(token, name);
      }
    }

    // Create child container
    const container = this.root.createChild(name);
    for (const registration of holder.getRegistrations()) {
      container.register(registration);
    }

    this.moduleContainers.push(container);
    return container;
  }

  /**
   * Starts the context: validate → resolve singletons → apply tracing.
   *
   * @throws {ContainerValidationError} if validation fails (excluding scope violations)
   * @throws {Error} if already started
   */
  async start(): Promise<void> {
    if (this.state !== 'idle') {
      throw new Error(`ContainerContext cannot start: current state is '${this.state}'`);
    }

    // Validate entire container tree
    const result = this.root.validate();
    if (!result.valid) {
      const errors = result.issues.filter((i) => i.type !== 'scope-violation');
      const scopeViolations = result.issues.filter((i) => i.type === 'scope-violation');
      if (scopeViolations.length > 0) {
        const logger = useLogger('putnami:di');
        for (const violation of scopeViolations) {
          logger.warn(violation.message);
        }
      }
      if (errors.length > 0) {
        throw new ContainerValidationError(errors);
      }
    }

    // Resolve all non-lazy singletons (runs async factories)
    await this.root.resolveAll();

    // Cache scoped providers for efficient scope() calls
    this.scopedProviderCache = collectScopedProviders(this.root, this.moduleContainers);

    // Apply tracing proxy if enabled globally
    if (this.options.proxy?.tracing) {
      const sink = this.options.traceSink ?? noopTraceSink;
      applyTracing(this.root, sink);
      for (const container of this.moduleContainers) {
        applyTracing(container, sink, container.name);
      }
    }

    this.state = 'started';
  }

  /**
   * Closes the context: dispose in reverse order.
   */
  async close(): Promise<void> {
    if (this.state !== 'started') {
      throw new Error(`ContainerContext cannot close: current state is '${this.state}'`);
    }

    this.state = 'closed';
    await this.root.close();
  }

  /**
   * Enables `await using ctx = new ContainerContext()` pattern.
   */
  async [Symbol.asyncDispose](): Promise<void> {
    if (this.state === 'started') {
      await this.close();
    }
  }

  /**
   * Resolves an instance from the context.
   * Searches root first, then module containers for public providers.
   *
   * @throws {ContainerClosedError} if the context is closed
   * @throws {NotRegisteredError} if the token is not found
   */
  get<T>(token: Token<T>): T {
    this.assertStarted('get');
    // Try root first
    if (this.root.has(token)) {
      return this.root.get(token);
    }
    // Try module containers — public providers only: a private provider
    // resolves only inside the container that declared it (private-visibility).
    for (const container of this.moduleContainers) {
      if (container.hasPublic(token)) {
        return container.get(token);
      }
    }
    // Not found in root or any module: throw a single module-aware error listing
    // every container that was searched. (A redundant `root.get()` here would
    // throw an error naming only the root and lose the module context.)
    const searched = [this.root.name, ...this.moduleContainers.map((c) => c.name)];
    throw new NotRegisteredError(token, this.root.name, [], searched);
  }

  /**
   * Lists all instances matching a filter.
   * Collects from root and all module containers.
   *
   * @example
   * ```typescript
   * const plugins = ctx.list<Plugin>({ tags: 'plugin' });
   * const secureMiddleware = ctx.list<Middleware>({ tags: ['http', 'secure'] });
   * ```
   */
  list<T = unknown>(filter: FilterOptions): T[] {
    this.assertStarted('list');
    const results = this.root.list<T>(filter);
    // Module containers are children of root, so container.list() walks up
    // to root and would double-collect root-level providers. Deduplicate.
    const seen = new Set<unknown>(results);
    for (const container of this.moduleContainers) {
      for (const item of container.listPublic<T>(filter)) {
        if (!seen.has(item)) {
          seen.add(item);
          results.push(item);
        }
      }
    }
    return results;
  }

  /**
   * Checks if a token is available in the context (root or any module).
   */
  has(token: Token): boolean {
    if (this.root.has(token)) return true;
    return this.moduleContainers.some((c) => c.hasPublic(token));
  }

  /**
   * Executes a function within a scoped container.
   * Creates a fresh scope for each invocation (HTTP request, job, event, etc.).
   *
   * The scope is stored in AsyncLocalStorage so nested async calls
   * can resolve scoped dependencies.
   */
  async scope<R>(fn: (scope: ScopeContext) => R | Promise<R>): Promise<R> {
    this.assertStarted('scope');

    const scopedContainer = new ScopedContainer('scope', this.root);

    // Register scoped providers from cache (computed once at start)
    if (this.scopedProviderCache) {
      for (const { provider } of this.scopedProviderCache) {
        scopedContainer.register({ __brand: 'Registration', provider });
      }
    }

    this.diDebugLogger?.onScopeCreate();

    const scopeContext = this.buildScopeContext(scopedContainer);

    const context = { [SCOPE_CONTAINER_KEY]: scopeContext };

    try {
      const result = await runInContext(context, async () => fn(scopeContext));
      return result as R;
    } finally {
      await scopedContainer.close();
      this.diDebugLogger?.onScopeClose();
    }
  }

  /**
   * Creates a detached scope without entering AsyncLocalStorage.
   *
   * Unlike `scope()`, this does NOT call `runInContext()`. The caller is
   * responsible for injecting the scope into their own async context
   * (e.g., by assigning `SCOPE_CONTAINER_KEY` to a context object before
   * passing it to `runInContext()`).
   *
   * This is essential when the caller already manages its own `runInContext()`
   * call (HTTP request handler, event handler) and needs DI scope support
   * without nesting `AsyncLocalStorage.run()` calls (which would replace
   * rather than merge context).
   *
   * @example
   * ```typescript
   * const { scope, close } = await ctx.createScope();
   * (httpContext as any)[SCOPE_CONTAINER_KEY] = scope;
   * try {
   *   await runInContext(httpContext, () => handler(httpContext));
   * } finally {
   *   await close();
   * }
   * ```
   */
  async createScope(): Promise<DetachedScope> {
    return this.createScopeSync();
  }

  /**
   * Synchronous variant of {@link createScope}.
   *
   * Scope construction is inherently synchronous (only {@link DetachedScope.close}
   * is async), so this lets latency-sensitive callers create a scope lazily from
   * a non-async context — e.g. an HTTP request that only materialises its DI scope
   * if a handler actually resolves something. {@link createScope} delegates here.
   */
  createScopeSync(): DetachedScope {
    this.assertStarted('createScope');

    const scopedContainer = new ScopedContainer('scope', this.root);

    if (this.scopedProviderCache) {
      for (const { provider } of this.scopedProviderCache) {
        scopedContainer.register({ __brand: 'Registration', provider });
      }
    }

    this.diDebugLogger?.onScopeCreate();

    return {
      scope: this.buildScopeContext(scopedContainer),
      close: async () => {
        await scopedContainer.close();
        this.diDebugLogger?.onScopeClose();
      },
    };
  }

  /**
   * Refreshes all dynamic providers across the entire container tree.
   */
  async refresh(): Promise<void> {
    this.assertStarted('refresh');
    await this.root.refreshDynamic();
    for (const container of this.moduleContainers) {
      await container.refreshDynamic();
    }
  }

  /**
   * Creates a fork of this context for testing.
   *
   * The fork copies all registrations and module structure, then allows
   * overriding specific providers with test doubles.
   */
  fork() {
    if (!_forkFactory) {
      throw new Error('ContainerContextFork not registered. Import container-context-fork first.');
    }
    return _forkFactory(this, this.options);
  }

  /**
   * Returns a debug-friendly description of the full container tree.
   * Useful for diagnosing DI issues.
   *
   * @param options - Pass `{ validate: true }` to include validation issues
   *   inline with the container description for a unified graph + issues view.
   *
   * @example
   * ```typescript
   * const info = ctx.describe({ validate: true });
   * // info.issues — validation issues for the root container
   * // info.children[0].issues — issues for child containers
   * ```
   */
  describe(options?: DescribeOptions): ContainerDescription {
    return this.root.describe(options);
  }

  /**
   * Statically analyze which request/session-scoped providers are transitively
   * reachable from a set of root tokens, without instantiating anything.
   *
   * Walks the declared dependency graph across the root container and every
   * mounted module. Used by the web framework to prove a `.static()` route does
   * not depend on request-scoped state (the proof comes from the graph, not from
   * which code paths happen to run during pre-render).
   *
   * Requires only that providers are registered (mount time); the context does
   * not need to be started.
   *
   * @param roots - The entry tokens to analyze (e.g. a route loader's `.inject()`).
   */
  analyzeScopeReachability(roots: readonly Token[]): ScopeReachability {
    const owner = new WeakMap<Provider, Container>();
    const scopedByToken = new Map<Token, Provider>();

    for (const [, provider] of this.root.getProviders()) {
      owner.set(provider, this.root);
      if (provider.scope === 'scoped') scopedByToken.set(provider.token, provider);
    }
    for (const mc of this.moduleContainers) {
      for (const [, provider] of mc.getProviders()) {
        owner.set(provider, mc);
        if (provider.scope === 'scoped') scopedByToken.set(provider.token, provider);
      }
    }

    const lookupFromScope = (token: Token): Provider | undefined => {
      // Loader `.inject()` resolves from the request scope. Scoped providers are
      // registered into that scope before falling back to root singletons.
      const scoped = scopedByToken.get(token);
      if (scoped) return scoped;
      return this.root.findVisibleProvider(token);
    };

    const lookupRoot = (token: Token): Provider | undefined => {
      const scopedOrRoot = lookupFromScope(token);
      if (scopedOrRoot) return scopedOrRoot;
      // Then each module's own registrations, in the same order as ScopeContext.
      for (const mc of this.moduleContainers) {
        const provider = mc.findPublicProvider(token);
        if (provider) return provider;
      }
      return undefined;
    };

    const lookup: ProviderLookup = (token, context) => {
      if (!context?.from) {
        return lookupRoot(token);
      }
      if (context.from.scope === 'scoped') {
        return lookupFromScope(token);
      }
      return owner.get(context.from)?.findVisibleProvider(token);
    };
    return analyzeScopeReachability(roots, lookup);
  }

  /**
   * Returns the root container. Used internally by @putnami/application.
   * @internal
   */
  getRoot(): Container {
    return this.root;
  }

  /**
   * Returns all module containers. Used internally by @putnami/application.
   * @internal
   */
  getModuleContainers(): Container[] {
    return this.moduleContainers;
  }

  /**
   * Returns all root-level registrations for fork/clone.
   * @internal
   */
  getRegistrations(): Registration[] {
    const registrations: Registration[] = [];
    for (const [, provider] of this.root.getProviders()) {
      registrations.push({ __brand: 'Registration', provider });
    }
    return registrations;
  }

  /**
   * Builds a ScopeContext that resolves from the scoped container first,
   * then falls back to module containers for providers registered via module().provide().
   */
  private buildScopeContext(scopedContainer: ScopedContainer): ScopeContext {
    return {
      get: <T>(token: Token<T>): T => {
        if (scopedContainer.has(token)) return scopedContainer.get(token);
        for (const mc of this.moduleContainers) {
          if (mc.hasPublic(token)) return mc.get(token);
        }
        // Fall through to scopedContainer.get() to throw NotRegisteredError
        return scopedContainer.get(token);
      },
      list: <T>(filter: FilterOptions): T[] => {
        const results = scopedContainer.list<T>(filter);
        const seen = new Set<unknown>(results);
        for (const mc of this.moduleContainers) {
          for (const item of mc.listPublic<T>(filter)) {
            if (!seen.has(item)) {
              seen.add(item);
              results.push(item);
            }
          }
        }
        return results;
      },
      has: (token: Token): boolean => {
        if (scopedContainer.has(token)) return true;
        return this.moduleContainers.some((mc) => mc.has(token));
      },
    };
  }

  private assertStarted(method: string): void {
    if (this.state === 'closed') {
      throw new ContainerClosedError('context');
    }
    if (this.state !== 'started') {
      throw new Error(`ContainerContext.${method}() requires the context to be started`);
    }
  }
}

// ---------------------------------------------------------------------------
// Fork factory — breaks circular dependency with container-context-fork.ts
// ---------------------------------------------------------------------------

type ForkFactory = (parent: ContainerContext, options: ContainerContextOptions) => ContainerContext;
let _forkFactory: ForkFactory | undefined;

/** @internal Register the fork constructor, called by container-context-fork.ts at import time. */
export function registerForkFactory(factory: ForkFactory): void {
  _forkFactory = factory;
}
