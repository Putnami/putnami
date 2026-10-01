import type {
  AsyncFactory,
  ContainerHolder,
  ProvideOptions,
  Registration,
  ResolveToken,
  SyncFactory,
  Token,
  Type,
} from '@putnami/runtime';
import { provide as createRegistration, tokenName } from '@putnami/runtime';
import type { Promisable } from '@putnami/utils';
import { getExternalCaller, getProjectRoot } from '@putnami/utils';
import type { DeclaredFeature, FeatureComposition, FeatureDefinition } from '../features/design-graph';
import type { SecurityGuard, SecurityOptions } from '../security/security.types';
import type { BuildOptions, Plugin } from './module.types';

// Lifecycle/plugin types live in module.types.ts; re-exported here so existing
// importers of `@putnami/application`'s module surface keep working.
export type { BuildOptions, GenerateResult, Plugin } from './module.types';

export interface ComposeModulesOptions {
  /**
   * Name for the composed module. Defaults to the child module names joined
   * with `+`.
   */
  name?: string;

  /**
   * Detect duplicate provider tokens across the composed subtree before the DI
   * container is built. Enabled by default.
   */
  detectDuplicates?: boolean;
}

export type HealthContributionKind = 'health' | 'readiness';

export type ResolvedTokenTuple<TDeps extends readonly Token[]> = {
  -readonly [Index in keyof TDeps]: TDeps[Index] extends Token ? ResolveToken<TDeps[Index]> : never;
};

export type InjectedHealthProbe<TDeps extends readonly Token[]> = (
  ...args: [...ResolvedTokenTuple<TDeps>, AbortSignal]
) => Promisable<void>;

export interface HealthContribution {
  readonly kind: HealthContributionKind;
  readonly name: string;
  readonly deps: readonly Token[];
  readonly probe: (...args: unknown[]) => Promisable<void>;
}

/**
 * A module groups related plugins, providers, and sub-modules into a composable unit.
 *
 * Modules form the composition hierarchy:
 * `Application > Module > Plugin`
 *
 * Modules implement {@link ContainerHolder} to participate in the DI container tree.
 * When `application.use(module)`, the module's registrations are mounted into the
 * container tree via {@link ContainerContext}.
 *
 * - **Modules are not startable** — only Application has a lifecycle.
 * - **Modules are composable** — modules can contain other modules.
 * - **Modules can use plugins** — plugins are collected by the Application for lifecycle management.
 *
 * @example
 * ```typescript
 * // Create a module with plugins and providers
 * const authModule = module('auth')
 *   .provide(AuthService, { deps: [Database] })
 *   .use(oauth())
 *   .use(api({ prefix: '/auth' }));
 *
 * // Compose into an application
 * const app = application()
 *   .provide(Database)
 *   .use(http())
 *   .use(authModule)
 *   .use(platform());
 * ```
 */
export class Module implements ContainerHolder {
  readonly name: string;
  private parent?: Module;
  private _path?: string;
  private children: Array<Plugin | Module> = [];
  private _registrations: Registration[] = [];
  private _requirements: Token[] = [];
  private _healthContributions: HealthContribution[] = [];
  private _shutdownHooks: (() => Promise<void>)[] = [];
  private _security?: SecurityOptions | SecurityGuard;
  private _feature?: DeclaredFeature;
  private _featureComposition?: FeatureComposition;

  /** Build options set during build phase, accessible by plugins */
  buildOptions?: BuildOptions;

  /**
   * Register DI-injected health/readiness probes owned by this module.
   *
   * The probe receives resolved dependencies followed by the endpoint's
   * AbortSignal. Contributed probes are discovered by platform() alongside
   * HealthChecker / ReadinessChecker plugins.
   */
  readonly health = {
    contribute: <const TDeps extends readonly Token[]>(
      name: string,
      deps: TDeps,
      probe: InjectedHealthProbe<TDeps>,
    ): this => {
      this._healthContributions.push({
        kind: 'health',
        name,
        deps: [...deps],
        probe: probe as (...args: unknown[]) => Promisable<void>,
      });
      return this;
    },

    contributeReadiness: <const TDeps extends readonly Token[]>(
      name: string,
      deps: TDeps,
      probe: InjectedHealthProbe<TDeps>,
    ): this => {
      this._healthContributions.push({
        kind: 'readiness',
        name,
        deps: [...deps],
        probe: probe as (...args: unknown[]) => Promisable<void>,
      });
      return this;
    },
  };

  constructor(name: string) {
    this.name = name;
  }

  /**
   * Set the base path for this module.
   * Child plugins (api, react, static) will inherit this path as their prefix
   * unless they specify an explicit prefix.
   *
   * @param value - The base path (e.g., '/tasks', '/auth')
   */
  path(value: string): this {
    this._path = value;
    return this;
  }

  /**
   * Declare the single product outcome implemented by this module. Native API,
   * event, data, migration, service, and generated-client facts are discovered
   * from their ordinary framework declarations.
   *
   * The optional composition selects native owners this feature also covers but
   * does not contain in the module tree — sibling modules composed elsewhere,
   * and source paths holding file routes a workload root plugin scans. It is
   * read at build time only and never affects composition or lifecycle.
   *
   * @example One declaration across a library composer and workload routes
   * ```typescript
   * module('opaque-tokens')
   *   .feature(
   *     { id: 'auth/opaque-tokens', name: 'Opaque tokens', outcome: '…', owner: 'auth' },
   *     { modules: [googleOauth], sources: ['src/api/auth/token'] },
   *   )
   * ```
   */
  feature(feature: FeatureDefinition, composition?: FeatureComposition): this {
    if (this._feature) {
      throw new Error(`Module '${this.name}' already declares feature '${this._feature.id}'`);
    }
    const caller = getExternalCaller(getProjectRoot());
    this._feature = {
      ...feature,
      ...(caller
        ? {
            provenance: {
              path: caller.filePath,
              line: caller.lineNumber,
              ...(caller.functionName ? { symbol: caller.functionName } : {}),
            },
          }
        : {}),
    };
    this._featureComposition = composition;
    return this;
  }

  /** @internal Build-time design graph discovery. */
  getFeature(): DeclaredFeature | undefined {
    return this._feature;
  }

  /** @internal Build-time design graph discovery of selected native owners. */
  getFeatureComposition(): FeatureComposition | undefined {
    return this._featureComposition;
  }

  /** @internal Feature inherited by this module from its closest ancestor. */
  getEffectiveFeature(): DeclaredFeature | undefined {
    return this._feature ?? this.parent?.getEffectiveFeature();
  }

  /**
   * @internal Build-time design graph discovery: does the composed tree declare
   * a feature anywhere? A workload root can own scanned routes that a feature
   * declared further down selects, so ancestor inheritance is too narrow a test.
   */
  declaresAnyFeature(): boolean {
    const root = this.getRoot();
    return Boolean(root.getFeature()) || root.collectModules().some((candidate) => Boolean(candidate.getFeature()));
  }

  /**
   * @internal Build-time design graph discovery: every source path a feature in
   * the composed tree selects. A plugin outside every feature scope uses this to
   * tell whether any of its declarations can reach the graph at all, instead of
   * doing discovery work for a feature that never selected it.
   */
  collectFeatureSourceSelections(): string[] {
    const root = this.getRoot();
    return [root, ...root.collectModules()]
      .filter((candidate) => Boolean(candidate.getFeature()))
      .flatMap((candidate) => [...(candidate.getFeatureComposition()?.sources ?? [])]);
  }

  /**
   * Get the base path for this module.
   * Returns undefined if no path was set.
   */
  getPath(): string | undefined {
    return this._path;
  }

  /**
   * Apply security to all handlers registered by plugins in this module.
   *
   * All endpoints, loaders, and actions in this module will be protected
   * by the given security rules. Individual handlers can add additional
   * requirements with their own `.secure()` calls.
   *
   * @example Require authentication for all routes in this module
   * ```typescript
   * module('dashboard')
   *   .secure()
   *   .use(api())
   * ```
   *
   * @example Require admin role for all routes
   * ```typescript
   * module('admin')
   *   .secure({ roles: ['admin'] })
   *   .use(api())
   * ```
   *
   * @example Custom guard function
   * ```typescript
   * module('org')
   *   .secure((user, ctx) => user.orgId === ctx.params?.orgId)
   *   .use(api())
   * ```
   */
  secure(optionsOrGuard?: SecurityOptions | SecurityGuard): this {
    this._security = optionsOrGuard ?? {};
    return this;
  }

  /**
   * Get the module-level security options.
   * Returns undefined if no security was set.
   */
  getSecurity(): SecurityOptions | SecurityGuard | undefined {
    return this._security;
  }

  /**
   * Registers a provider in this module.
   * Supports all `provide()` overloads.
   */
  provide<T extends object>(classRef: Type<T>): this;
  provide<T extends object>(classRef: Type<T>, options: ProvideOptions<T>): this;
  provide<T>(token: Token<T>, factory: SyncFactory<T>): this;
  provide<T>(token: Token<T>, factory: SyncFactory<T>, options: ProvideOptions<T>): this;
  provide<T>(token: Token<T>, factory: AsyncFactory<T>): this;
  provide<T>(token: Token<T>, factory: AsyncFactory<T>, options: ProvideOptions<T>): this;
  provide(...args: unknown[]): this {
    const registration = createRegistration(...(args as Parameters<typeof createRegistration>));
    this._registrations.push(registration);
    return this;
  }

  /**
   * Registers a pre-built Registration in this module.
   *
   * Unlike `provide()` which creates a Registration from arguments,
   * this method accepts an already-built Registration (e.g. from `provideConfig()`).
   *
   * @example
   * ```typescript
   * import { provideConfig } from '@putnami/runtime';
   * import { DatabaseConfig } from './database.config';
   *
   * const app = application()
   *   .register(provideConfig(DatabaseConfig));
   * ```
   *
   * Generic in the provided type, like `ContainerContext.register`. A
   * `Registration<T>` is not assignable to `Registration<unknown>` — its
   * provider's factory and `onClose` make it invariant — so the non-generic
   * signature rejected every typed helper, including the documented
   * `register(provideRepository(UsersTable))`.
   */
  register<T>(registration: Registration<T>): this {
    this._registrations.push(registration as Registration);
    return this;
  }

  /**
   * Declares that this module requires a token to be provided by its parent.
   * Validated when the module is mounted into the ContainerContext.
   */
  require<T>(token: Token<T>): this {
    this._requirements.push(token);
    return this;
  }

  /**
   * Add a plugin or sub-module.
   *
   * - **Plugin**: Registered for lifecycle management (generate → warmup → start → stop).
   * - **Module**: Composed as a child module. Its plugins and providers participate
   *   in the parent Application's lifecycle.
   */
  use(child: Plugin | Module): this {
    if (!child) {
      throw new Error('Plugin or module not provided');
    }
    if (child instanceof Module) {
      child.parent = this;
    }
    this.children.push(child);
    return this;
  }

  /**
   * Register a shutdown hook.
   * Hooks are called in reverse order during Application stop().
   */
  onStop(hook: () => Promise<void>): this {
    this._shutdownHooks.push(hook);
    return this;
  }

  /** @internal Build-time observation of the native module hook surface. */
  hasShutdownHooks(): boolean {
    return this._shutdownHooks.length > 0;
  }

  /**
   * Get all direct plugins (not from sub-modules).
   */
  getPlugins(): Plugin[] {
    return this.children.filter((c): c is Plugin => !(c instanceof Module));
  }

  /**
   * Get all direct sub-modules.
   */
  getModules(): Module[] {
    return this.children.filter((c): c is Module => c instanceof Module);
  }

  /**
   * Get a plugin by its constructor type.
   * Searches local plugins first, then walks up the parent chain.
   *
   * @throws Error if the plugin is not found anywhere in the hierarchy
   */
  getPlugin<P extends Plugin>(type: Type<P>): P {
    const plugin = this.findPlugin(type);
    if (!plugin) {
      throw new Error(`Plugin ${type.name} not found. Add it with .use(new ${type.name}())`);
    }
    return plugin;
  }

  /**
   * Ensure a plugin exists, creating and warming it up if missing.
   * Searches local plugins and parent chain. Creates locally if not found.
   *
   * @example
   * ```typescript
   * class MyPlugin implements Plugin {
   *   async warmup(owner: Module) {
   *     // Creates HttpPlugin if not registered anywhere in the hierarchy
   *     const http = await owner.ensurePlugin(HttpPlugin);
   *     http.get('/my-route', handler);
   *   }
   * }
   * ```
   */
  async ensurePlugin<P extends Plugin>(type: Type<P>): Promise<P> {
    let plugin = this.findPlugin(type);
    if (!plugin) {
      plugin = new type();
      // Insert at the beginning so it's before dependent plugins
      this.children.unshift(plugin);
      await plugin.warmup?.(this);
    }
    return plugin;
  }

  /**
   * Find a plugin by type, searching local plugins then walking up the parent chain.
   * Returns undefined if not found.
   */
  findPlugin<P extends Plugin>(type: Type<P>): P | undefined {
    // Search local plugins (not sub-modules)
    for (const child of this.children) {
      if (!(child instanceof Module) && child instanceof type) {
        return child as P;
      }
    }
    // Walk up parent chain
    if (this.parent) {
      return this.parent.findPlugin(type);
    }
    return undefined;
  }

  /**
   * Returns the root of the module tree. Plugins use this from warmup
   * to reach framework-owned resources (the per-app MigrationRegistry,
   * etc.) before the DI container has been built.
   */
  getRoot(): Module {
    return this.parent ? this.parent.getRoot() : this;
  }

  /**
   * Collect all plugins from this module and its sub-modules, depth-first.
   * Each plugin is paired with its owning module.
   *
   * The order preserves registration order: for each `.use()` call,
   * if it's a sub-module, its plugins are collected recursively first;
   * if it's a plugin, it's added directly.
   *
   * @internal Used by Application for lifecycle management
   */
  collectPlugins(): Array<{ plugin: Plugin; owner: Module }> {
    const result: Array<{ plugin: Plugin; owner: Module }> = [];
    for (const child of this.children) {
      if (child instanceof Module) {
        result.push(...child.collectPlugins());
      } else {
        result.push({ plugin: child, owner: this });
      }
    }
    return result;
  }

  /**
   * Collect health/readiness probe contributions from this module tree.
   *
   * @internal Used by platform() for probe discovery.
   */
  collectHealthContributions(): HealthContribution[] {
    const result = [...this._healthContributions];
    for (const child of this.children) {
      if (child instanceof Module) {
        result.push(...child.collectHealthContributions());
      }
    }
    return result;
  }

  /**
   * Collect all shutdown hooks from this module and its sub-modules.
   *
   * @internal Used by Application during shutdown
   */
  collectShutdownHooks(): Array<() => Promise<void>> {
    const result: Array<() => Promise<void>> = [];
    for (const child of this.children) {
      if (child instanceof Module) {
        result.push(...child.collectShutdownHooks());
      }
    }
    result.push(...this._shutdownHooks);
    return result;
  }

  /**
   * Collect all ContainerHolders (this module and sub-modules) in depth-first order.
   * Used by Application to mount all modules into the ContainerContext.
   *
   * @internal
   */
  collectModules(): Module[] {
    const result: Module[] = [];
    for (const child of this.children) {
      if (child instanceof Module) {
        result.push(child);
        result.push(...child.collectModules());
      }
    }
    return result;
  }

  /**
   * Collect modules that should be mounted as independent DI containers.
   *
   * Most modules mount one container per module. Composed modules override this
   * so their child providers mount as a single requirement-resolution unit while
   * the ordinary module tree remains available to plugin lifecycle walks.
   *
   * @internal
   */
  collectContainerModules(): Module[] {
    const result: Module[] = [];
    for (const child of this.children) {
      if (child instanceof Module) {
        result.push(child);
        result.push(...child.collectContainerModules());
      }
    }
    return result;
  }

  /**
   * Returns all DI registrations in this module.
   * Part of the ContainerHolder interface.
   */
  getRegistrations(): readonly Registration[] {
    return this._registrations;
  }

  /**
   * Returns all DI requirements in this module.
   * Part of the ContainerHolder interface.
   */
  getRequirements(): readonly Token[] {
    return this._requirements;
  }
}

/**
 * Creates a new module.
 *
 * @param name - The module name (used in error messages and debugging)
 * @returns A new Module instance
 *
 * @example
 * ```typescript
 * const authModule = module('auth')
 *   .path('/auth')
 *   .provide(AuthService, { deps: [Database] })
 *   .use(oauth())
 *   .use(api());
 * ```
 */
export function module(name: string): Module {
  return new Module(name);
}

interface RegistrationRecord {
  registration: Registration;
  owner: Module;
}

/**
 * A module that preserves child modules for plugin discovery while mounting
 * their providers as a single DI unit.
 */
export class ComposedModule extends Module {
  private readonly detectDuplicates: boolean;

  constructor(modules: readonly Module[], options: ComposeModulesOptions = {}) {
    if (modules.length === 0) {
      throw new Error('composeModules() requires at least one module');
    }

    super(options.name ?? modules.map((mod) => mod.name).join('+'));
    this.detectDuplicates = options.detectDuplicates ?? true;

    for (const mod of modules) {
      this.use(mod);
    }

    if (this.detectDuplicates) {
      this.assertNoDuplicateProviders(this.collectRegistrationRecords());
    }
  }

  override getRegistrations(): readonly Registration[] {
    const records = this.collectRegistrationRecords();
    if (this.detectDuplicates) {
      this.assertNoDuplicateProviders(records);
    }
    return records.map(({ registration }) => registration);
  }

  override getRequirements(): readonly Token[] {
    const records = this.collectRegistrationRecords();
    if (this.detectDuplicates) {
      this.assertNoDuplicateProviders(records);
    }

    const internalProviders = new Set<Token>(records.map(({ registration }) => registration.provider.token));
    const requirements: Token[] = [];
    const seen = new Set<Token>();
    const addRequirement = (token: Token): void => {
      if (!seen.has(token)) {
        seen.add(token);
        requirements.push(token);
      }
    };

    for (const token of super.getRequirements()) {
      addRequirement(token);
    }

    for (const mod of this.collectCompositionModules()) {
      for (const token of mod.getRequirements()) {
        if (!internalProviders.has(token)) {
          addRequirement(token);
        }
      }
    }

    return requirements;
  }

  override collectContainerModules(): Module[] {
    return [];
  }

  private collectCompositionModules(): Module[] {
    const modules: Module[] = [];
    for (const child of this.getModules()) {
      modules.push(child);
      modules.push(...child.collectContainerModules());
    }
    return modules;
  }

  private collectRegistrationRecords(): RegistrationRecord[] {
    const records: RegistrationRecord[] = [];

    for (const registration of super.getRegistrations()) {
      records.push({ registration, owner: this });
    }

    for (const mod of this.collectCompositionModules()) {
      for (const registration of mod.getRegistrations()) {
        records.push({ registration, owner: mod });
      }
    }

    return records;
  }

  private assertNoDuplicateProviders(records: RegistrationRecord[]): void {
    const seen = new Map<Token, Module>();
    for (const { registration, owner } of records) {
      const token = registration.provider.token;
      const previous = seen.get(token);
      if (previous) {
        throw new Error(
          `Duplicate provider for ${tokenName(token)} in composed module '${this.name}' from '${previous.name}' and '${owner.name}'`,
        );
      }
      seen.set(token, owner);
    }
  }
}

export function composeModules(modules: readonly Module[], options?: ComposeModulesOptions): ComposedModule {
  return new ComposedModule(modules, options);
}
