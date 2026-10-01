/**
 * A constructor type that can be instantiated with `new`.
 *
 * @typeParam T - The instance type created by the constructor
 */
// biome-ignore lint/suspicious/noExplicitAny: Required for constructor signature
export type Type<T = any> = new (..._args: any[]) => T;

/**
 * A branded string token carrying a phantom type for type-safe resolution.
 *
 * @typeParam T - The type of the value associated with this token
 */
export interface NamedToken<T = unknown> {
  readonly __brand: 'NamedToken';
  readonly __type: T;
  readonly name: string;
}

/**
 * A tag selector for resolving all providers matching a tag.
 * Used with `list()` to retrieve multiple instances.
 *
 * TagSelector implements FilterOptions so it can be used directly
 * with `resolveInjection()` for type-safe multi-resolution.
 *
 * @typeParam T - The type of each tagged instance
 */
export interface TagSelector<T = unknown> extends FilterOptions {
  readonly __brand: 'TagSelector';
  readonly __type: T;
  readonly tags: string;
}

/**
 * A reference to an injectable class, named token, or symbol.
 * Used to register and retrieve instances from the DI container.
 *
 * @typeParam T - The type of the referenced instance
 */
export type Token<T = unknown> = Type<T> | NamedToken<T> | symbol;

/**
 * Resolves the instance type for a given token or filter.
 *
 * - Class token → instance type
 * - Named token → phantom type
 * - TagSelector → array of phantom type
 * - FilterOptions → unknown[] (no phantom type)
 */
export type ResolveToken<T> =
  T extends Type<infer I>
    ? I
    : T extends NamedToken<infer V>
      ? V
      : T extends TagSelector<infer V>
        ? V[]
        : T extends FilterOptions
          ? unknown[]
          : never;

/**
 * Maps an object of tokens/filters to their resolved types.
 */
export type ResolvedMap<M extends Record<string, Token | TagSelector | FilterOptions>> = {
  [K in keyof M]: ResolveToken<M[K]>;
};

/**
 * Defines the lifecycle scope of a provider.
 *
 * - `'singleton'`: Single instance shared across the entire application (default)
 * - `'scoped'`: New instance per scope (HTTP request, job, event, worker message)
 */
export type Scope = 'singleton' | 'scoped';

/**
 * Defines the visibility of a provider within the container hierarchy.
 *
 * - `'public'`: Visible to all containers in the hierarchy (default)
 * - `'private'`: Only visible within the container that registered it
 */
export type Visibility = 'private' | 'public';

/**
 * Configuration for proxy wrapping.
 */
export interface ProxyOptions {
  /** Enable method-call tracing */
  tracing?: boolean;
  /** Enable telemetry metrics */
  telemetry?: boolean;
}

/**
 * Options for provider registration.
 *
 * @typeParam T - The type of the provided instance
 */
export interface ProvideOptions<T = unknown> {
  /** Constructor dependencies (for class providers without a factory) */
  deps?: Token[];
  /** Lifecycle scope. Default: `'singleton'` */
  scope?: Scope;
  /** Visibility in the container hierarchy. Default: `'public'` */
  visibility?: Visibility;
  /** Tags for grouped resolution via `list({ tags: '...' })` */
  tags?: string[];
  /** Dispose hook called during container `close()` */
  onClose?: (instance: T) => void | Promise<void>;
  /** Force proxy wrapping for tracing/telemetry */
  proxy?: boolean | ProxyOptions;
  /**
   * Asserts that {@link deps} fully enumerates this provider's dependencies, so
   * static dependency-graph analysis (e.g. the web static-render scope proof)
   * can treat the provider as decidable.
   *
   * Class providers are always complete — their synthesized factory resolves
   * exactly `deps`. Factory providers are conservatively treated as **incomplete**
   * unless you opt in here, because a factory body may resolve tokens that are
   * not listed in `deps`. Set this to `true` only when `deps` lists every token
   * the factory resolves.
   */
  depsComplete?: boolean;
  /**
   * Marks this provider as dynamic. Dynamic providers can be refreshed
   * at runtime via `context.refresh()`, causing their factory to re-execute
   * and the cached instance to be replaced.
   *
   * **Important:** refresh only replaces the dynamic provider's own instance.
   * Singletons that already injected the old instance keep their reference.
   * To observe the new value, dependents should either:
   * - Also be dynamic (so they get refreshed too)
   * - Use a scoped provider (re-created per scope, picks up the latest)
   * - Resolve the token lazily at call time rather than at construction
   */
  dynamic?: boolean;
  /**
   * Defer resolution until first access instead of resolving eagerly during `start()`.
   *
   * A lazy provider is not instantiated during `resolveAll()`. Instead,
   * a transparent proxy is returned on first `get()` that triggers the
   * actual factory on first property access.
   *
   * Useful for expensive services that may not be needed in every code path.
   */
  lazy?: boolean;
}

/**
 * A resolve function provided to factory providers for dependency resolution.
 *
 * Callable directly for single-token resolution:
 * ```typescript
 * provide(Service, (resolve) => {
 *   const db = resolve(Database);
 *   return new Service(db);
 * })
 * ```
 *
 * Use `.all()` for filter-based multi-resolution:
 * ```typescript
 * provide(PluginManager, (resolve) => {
 *   const plugins = resolve.all<Plugin>({ tags: 'plugin' });
 *   return new PluginManager(plugins);
 * })
 * ```
 */
export interface ResolveFn {
  <D>(token: Token<D>): D;
  all<D = unknown>(filter: FilterOptions): D[];
}

/**
 * A synchronous factory function.
 */
export type SyncFactory<T> = ((resolve: ResolveFn) => T) | (() => T);

/**
 * An asynchronous factory function.
 */
export type AsyncFactory<T> = (resolve: ResolveFn) => Promise<T>;

/**
 * Any factory function (sync or async).
 */
export type Factory<T> = SyncFactory<T> | AsyncFactory<T>;

/**
 * Internal provider descriptor stored by the container.
 *
 * @internal
 */
export interface Provider<T = unknown> {
  /** The token this provider is registered under */
  token: Token<T>;
  /** The factory function to create the instance */
  factory: Factory<T>;
  /** Whether the factory is async */
  async: boolean;
  /** Declared dependencies (for static graph analysis) */
  deps: Token[];
  /**
   * Whether {@link deps} is known to fully capture this provider's dependencies.
   * `true` for class providers (their synthesized factory resolves only `deps`)
   * and for factory providers that opted in via `ProvideOptions.depsComplete`.
   * When `false`, static graph analysis treats the provider as opaque — its
   * factory may resolve undeclared tokens, so its subtree cannot be fully proven.
   */
  depsComplete: boolean;
  /** Lifecycle scope */
  scope: Scope;
  /** Visibility in the container hierarchy */
  visibility: Visibility;
  /** Tags for grouped resolution */
  tags: string[];
  /** Dispose hook */
  onClose?: (instance: T) => void | Promise<void>;
  /** Proxy options */
  proxy?: boolean | ProxyOptions;
  /** Whether this provider can be refreshed at runtime */
  dynamic?: boolean;
  /** Defer resolution until first property access */
  lazy?: boolean;
}

/**
 * A registration returned by `provide()`. Inert data — no side effects until
 * registered with a container.
 *
 * @typeParam T - The type of the provided instance
 */
export interface Registration<T = unknown> {
  readonly __brand: 'Registration';
  readonly provider: Provider<T>;
}

/**
 * A single validation issue found during `start()`.
 */
export interface ValidationIssue {
  /** The type of issue */
  type: 'circular-dependency' | 'missing-dependency' | 'scope-violation' | 'duplicate-provider' | 'requirement-not-met';
  /** Human-readable description */
  message: string;
  /** The token(s) involved */
  tokens: Token[];
  /** The container where the issue was found */
  container: string;
}

/**
 * Result of container validation.
 */
export interface ValidationResult {
  valid: boolean;
  issues: ValidationIssue[];
}

/**
 * Filter options for `list()`. Extensible — starts with tag-based filtering.
 *
 * @example
 * ```typescript
 * context.list<Plugin>({ tags: 'plugin' });
 * context.list<Middleware>({ tags: ['http', 'auth'] });
 * ```
 */
export interface FilterOptions {
  /** One or more tags to match. When an array, matches providers that have ALL specified tags. */
  tags?: string | string[];
}

/**
 * Interface for the scoped container passed to `scope()` callbacks.
 */
export interface ScopeContext {
  get<T>(token: Token<T>): T;
  list<T>(filter: FilterOptions): T[];
  has(token: Token): boolean;
}

/**
 * A detached scope created by `ContainerContext.createScope()`.
 *
 * Unlike `scope()`, a detached scope does NOT enter `AsyncLocalStorage`.
 * The caller is responsible for injecting `scope` into their own async
 * context (e.g., by assigning `SCOPE_CONTAINER_KEY` to a context object
 * before passing it to `runInContext()`).
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
export interface DetachedScope {
  /** The scope context for resolving dependencies */
  scope: ScopeContext;
  /** Closes the scope and disposes scoped instances */
  close: () => Promise<void>;
}

/**
 * Options for container context configuration.
 */
export interface ContainerContextOptions {
  /** Global proxy settings applied to all providers */
  proxy?: ProxyOptions;
  /** Custom trace sink. When set with `proxy.tracing: true`, all providers emit traces here. */
  traceSink?: TraceSink;
  /**
   * Enable DI debug logging. When `true`, logs resolution events, scope creation,
   * and scope proxy creation to the `putnami:di` logger.
   *
   * Useful for understanding DI behavior during development.
   * Has no effect in production (`NODE_ENV=production`).
   */
  debug?: boolean;
}

/**
 * Interface for objects that hold DI registrations and requirements.
 * Implemented by Module and Application in `@putnami/application`.
 */
export interface ContainerHolder {
  /** All provider registrations declared on this holder */
  getRegistrations(): readonly Registration[];
  /** All token requirements this holder declares from its parent */
  getRequirements(): readonly Token[];
}

/**
 * Interface for trace emission.
 */
export interface TraceSink {
  emit(data: TraceData): void;
}

/**
 * Data emitted by tracing proxies.
 */
export interface TraceData {
  token: string;
  module?: string;
  method: string | symbol;
  duration: number;
  error?: unknown;
}
