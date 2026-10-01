import {
  emitInfraRequirements,
  emitMigrationBundle,
  isMigrationContributor,
  MigrationRegistry,
  type MigrationSource,
} from '@putnami/migration';
import {
  type ContainerContext,
  type ContainerContextOptions,
  type ContainerDescription,
  getRegisteredConfigDefinitions,
  installExceptionHandler,
  isConfigContributor,
  type Registration,
  registerContributedConfig,
  useLogger,
} from '@putnami/runtime';
import { getProjectRoot } from '@putnami/utils';
import { markStartupFailureLogged } from './app-bootstrap';
import { emitDesignGraph } from '../features/design-graph';
import { createCapabilitiesProducer, invalidateCapabilityManifest } from '../capabilities/capabilities.producer';
import { propagateBuildOptions, runGenerate, runPostGenerate } from './app-build';
import { buildAppContainerContext } from './app-container';
import { closeContainerContext, runShutdownHooks, stopPlugins } from './app-shutdown';
import { installSignalHandlers } from './app-signals';
import { type BuildOptions, type GenerateResult, Module, type Plugin } from './module';

export {
  ComposedModule,
  composeModules,
  type ComposeModulesOptions,
  type BuildOptions,
  type GenerateResult,
  type HealthContribution,
  type HealthContributionKind,
  type InjectedHealthProbe,
  Module,
  module,
  type Plugin,
  type ResolvedTokenTuple,
} from './module';

/**
 * Unified Application class for services, workers, and jobs.
 * Extends Module with a startable lifecycle and DI container context.
 *
 * Hierarchy: Application > Module > Plugin
 *
 * The Application manages two concerns:
 * 1. **Plugin lifecycle**: generate → warmup → start → stop
 * 2. **DI container**: providers are mounted into a {@link ContainerContext}
 *    that is built lazily after warmup (or on first DI access).
 *
 * @example HTTP Service
 * ```typescript
 * const app = application()
 *   .use(http({ port: 3000 }))
 *   .use(staticFiles());
 *
 * await app.start();
 * ```
 *
 * @example With modules and DI
 * ```typescript
 * const authModule = module('auth')
 *   .require(Database)
 *   .provide(AuthService, { deps: [Database] })
 *   .use(oauth())
 *   .use(api({ prefix: '/auth' }));
 *
 * const app = application()
 *   .provide(Database)
 *   .use(http())
 *   .use(authModule)
 *   .use(platform());
 *
 * await app.start();
 * const db = app.context.get(Database);
 * ```
 *
 * @example Job (one-shot)
 * ```typescript
 * const job = application()
 *   .use(sql())
 *   .run(async () => {
 *     await runMigrations();
 *   });
 *
 * await job.start(); // Exits when done
 * ```
 */
export class Application extends Module {
  private runner?: () => Promise<void>;
  private running = false;
  private _context?: ContainerContext;
  private _contextOptions: ContainerContextOptions;
  private _removeExceptionHandler?: () => void;
  private _removeSignalHandlers?: () => void;
  private _migrationRegistry = new MigrationRegistry();

  constructor(options: ContainerContextOptions = {}) {
    super('app');
    this._contextOptions = options;
    // Publish the per-app MigrationRegistry into DI so feature plugins
    // resolve the same instance the lifecycle drives.
    this.provide(MigrationRegistry, () => this._migrationRegistry);
  }

  /**
   * Returns the per-application MigrationRegistry. Plugins typically
   * resolve this from DI; the migrate CLI reads it directly after
   * `prepare()` to dispatch subcommands.
   */
  getMigrationRegistry(): MigrationRegistry {
    return this._migrationRegistry;
  }

  /**
   * The DI container context. Built lazily after warmup or on first access.
   *
   * Provides `get()`, `list()`, `has()`, `scope()`, and `refresh()` for
   * resolving dependencies across the entire application and its modules.
   *
   * @throws {Error} if accessed before the context has been built (before start)
   */
  get context(): ContainerContext {
    if (!this._context) {
      this._context = buildAppContainerContext(
        this._contextOptions,
        this.getRegistrations(),
        this.collectContainerModules(),
      );
    }
    return this._context;
  }

  /**
   * Registers a provider and invalidates the cached context.
   * Ensures that providers added after an early `context` access are included.
   */
  override provide(...args: unknown[]): this {
    this._context = undefined;
    // biome-ignore lint/suspicious/noExplicitAny: Forwarding overloaded arguments
    return super.provide(...(args as [any, any, any]));
  }

  /**
   * Registers a pre-built Registration and invalidates the cached context.
   *
   * Declared generic rather than through `Parameters<Module['register']>`: that
   * helper instantiates the base method's type parameter as `unknown`, which
   * would drop the provided type here and reject typed helpers such as
   * `provideRepository(UsersTable)`.
   */
  override register<T>(registration: Registration<T>): this {
    this._context = undefined;
    return super.register(registration);
  }

  /**
   * Adds a plugin or sub-module and invalidates the cached context.
   * Ensures that modules added after an early `context` access are mounted.
   */
  override use(child: Plugin | Module): this {
    this._context = undefined;
    return super.use(child);
  }

  /**
   * Define the main application runner.
   * This is the entry point for your application logic.
   *
   * - For jobs: Execute task and resolve (process exits)
   * - For custom logic that runs after all plugins start
   */
  run(runner: () => Promise<void>): this {
    this.runner = runner;
    return this;
  }

  /**
   * Check if the application is currently running.
   */
  isRunning(): boolean {
    return this.running;
  }

  /**
   * Mark the application as running without going through the full lifecycle.
   * Useful in tests that manually call warmup/start on individual plugins.
   */
  markAsRunning(): void {
    this.running = true;
  }

  /**
   * Build phase: run generate() on all plugins across the module tree, then
   * postGenerate() once every spec/loader is written. Collects assets and
   * exports from both passes, then emits the migration infra requirements and
   * bundle from contributed sources. Capability and optional feature-evidence
   * publication is the final build-only step; feature metadata is never read by
   * application composition, dependency injection, or the runtime lifecycle.
   */
  async build(options?: BuildOptions): Promise<GenerateResult> {
    this.buildOptions = options;
    propagateBuildOptions(this.getModules(), options);
    const plugins = this.collectPlugins();
    const publishCapabilityManifest = options?.publishCapabilityManifest !== false;
    const publishDesignGraph = options?.publishDesignGraph ?? publishCapabilityManifest;
    if (publishCapabilityManifest) {
      // Invalidate first and publish last: no failed/partial generate pass can
      // leave a stale capability manifest that appears to describe this build.
      invalidateCapabilityManifest();
    }
    const result = await runGenerate(plugins);

    // Second pass: derived codegen that reads artifacts emitted above. Runs
    // after the parallel generate() barrier so e.g. the client generator never
    // races the OpenAPI plugin writing its configured openapi.json output.
    const postResult = await runPostGenerate(plugins, result);

    await this.emitMigrationArtifacts(plugins);

    // Capability inventory consumes all concrete artifacts from both passes.
    // Only server loaders cross from postGenerate: generated client loaders are
    // outputs, not importable application modules.
    const capabilityInput: GenerateResult = {
      assets: { ...result.assets, ...postResult.assets },
      exports: { ...result.exports },
    };
    for (const [name, path] of Object.entries(postResult.exports ?? {})) {
      if (name.endsWith('-loader') && !name.endsWith('client-loader')) {
        capabilityInput.exports![name] = path;
      }
    }
    const capabilityResult = await createCapabilitiesProducer({ publishCapabilityManifest }).postGenerate?.(
      this,
      capabilityInput,
    );
    result.assets ??= {};
    result.exports ??= {};
    Object.assign(result.assets, postResult.assets);
    Object.assign(result.exports, postResult.exports);
    result.httpRoutes ??= [];
    result.httpRoutes.push(...(postResult.httpRoutes ?? []));
    Object.assign(result.assets, capabilityResult?.assets);
    Object.assign(result.exports, capabilityResult?.exports);
    if (publishDesignGraph) await emitDesignGraph(this);
    return result;
  }

  /**
   * Register every `ConfigContributor` plugin's config definitions into the
   * global config registry. The config dual of the `MigrationContributor` walk:
   * walking the composed plugin tree lets a workload publish its dependencies'
   * config — and the secrets their sensitive fields declare — transitively, so
   * config extraction and infra-requirements pick them up without per-workload
   * restatement. Throws when a dependency claims a path the workload (or another
   * dependency) already defines.
   *
   * Call after the workload's own `configToken()` calls have run (i.e. after
   * `build()` and route-loader imports) so a genuine path collision is caught.
   */
  registerContributedConfigs(): void {
    for (const { plugin } of this.collectPlugins()) {
      if (isConfigContributor(plugin)) {
        for (const def of plugin.configDefinitions()) {
          registerContributedConfig(def);
        }
      }
    }
  }

  /**
   * Built-in migration emission, the TypeScript counterpart of Go's
   * `app.Describe` migration describers: after generate runs, walk the
   * MigrationContributor plugins and emit the per-project infra requirements
   * (`.gen/infra/migration.json`) and migration bundle (`.gen/migration-bundle/`)
   * straight from the contributed sources. Source-driven, so it needs no
   * per-workload wiring and does not depend on the runtime persistence backend.
   *
   * Runs only when sources are actually contributed, so apps without migrations
   * are unaffected, and only when the build carries a putnami project identity
   * (`BuildOptions.projectName`) with `publishMigrationArtifacts` not disabled.
   * The build-generate hook is the single writer of both artifacts: it is the
   * only caller that passes the identity. Project root resolution stays
   * non-fatal for a `build()` without one — a test composing the app's
   * migration sources, a script — the build completes, it just emits no
   * migration artifacts.
   */
  private async emitMigrationArtifacts(plugins: Array<{ plugin: Plugin; owner: Module }>): Promise<void> {
    const sources: MigrationSource[] = [];
    for (const { plugin } of plugins) {
      if (isMigrationContributor(plugin)) {
        sources.push(...plugin.migrationSources());
      }
    }
    if (sources.length === 0) return;
    if (this.buildOptions?.publishMigrationArtifacts === false) return;
    const appName = this.buildOptions?.projectName;
    if (!appName) {
      useLogger('putnami').debug(
        'migration artifacts not emitted: build() ran without a putnami project identity (BuildOptions.projectName); only the build-generate hook emits them',
      );
      return;
    }

    let projectRoot: string;
    try {
      projectRoot = getProjectRoot();
    } catch (error) {
      const detail = error instanceof Error ? error.message : String(error);
      useLogger('putnami').warn(`migration artifacts not emitted: ${detail}`);
      return;
    }

    // The bundle first: a bundle the runner would refuse fails the build
    // before any infra fragment names its schemas.
    await emitMigrationBundle(sources, appName, projectRoot);
    emitInfraRequirements(sources, projectRoot);
  }

  /**
   * Prepare the application without starting it: run warmup on every
   * plugin, then collect MigrationContributor sources into the per-app
   * MigrationRegistry. After `prepare()` returns, the DI context is
   * built and the registry has every contributed Source registered —
   * the migrate CLI uses this state to dispatch subcommands directly
   * against the registry without running `start()`.
   *
   * `prepare()` and `start()` are mutually exclusive on the same
   * Application; `start()` invokes `prepare()` internally.
   */
  async prepare(): Promise<void> {
    const warmupPlugins = this.collectPlugins();

    // Warmup plugins sequentially in tree order so runners can register
    // themselves into the MigrationRegistry before sources are collected.
    for (const { plugin, owner } of warmupPlugins) {
      // biome-ignore lint/performance/noAwaitInLoops: warmup ordering matters
      await plugin.warmup?.(owner);
    }

    // After warmup: collect contributions and finalize the DI context.
    const allPlugins = this.collectPlugins();
    for (const { plugin } of allPlugins) {
      if (!isMigrationContributor(plugin)) continue;
      for (const source of plugin.migrationSources()) {
        this._migrationRegistry.addSource(source);
      }
    }

    // Invalidate cached context so it reflects providers/modules
    // registered before or during warmup, then bring it up.
    this._context = undefined;
    if (this.hasRegistrations()) {
      await this.context.start();
    }
  }

  /**
   * Start the application lifecycle:
   * 1. Prepare (warmup + collect migration sources + build DI context)
   * 2. Migrate phase: invoke plugin.migrate hooks, then registry.applyAll
   *    (source-only metadata is tolerated; runners no-op unless their
   *    AutoApply is set or Force is passed)
   * 3. Start all plugins (in parallel)
   * 4. Execute the runner (if defined)
   */
  async start(): Promise<void> {
    const logger = useLogger('putnami');
    const startedAt = Date.now();
    let contextStarted = false;
    let startedPlugins: Array<{ plugin: Plugin; owner: Module }> = [];

    try {
      await this.prepare();
      contextStarted = true;

      logger.debug(`🔥 warmed up`, { durationMs: Date.now() - startedAt });

      const allPlugins = this.collectPlugins();

      // Migrate phase: optional per-plugin hook, then aggregated apply.
      for (const { plugin, owner } of allPlugins) {
        // biome-ignore lint/performance/noAwaitInLoops: migrate ordering matters
        await plugin.migrate?.(owner);
      }
      await this._migrationRegistry.applyAll({ allowSourceOnly: true });

      // Start all plugins in parallel (post-prepare snapshot)
      const startEntries = allPlugins
        .filter(({ plugin }) => typeof plugin.start === 'function')
        .map(({ plugin, owner }) => ({ plugin, owner }));
      const startedPluginSet = new Set<Plugin>();
      const startResults = await Promise.allSettled(
        startEntries.map(async (entry) => {
          await entry.plugin.start?.(entry.owner);
          startedPluginSet.add(entry.plugin);
        }),
      );
      // Only stop plugins that actually finished starting — calling stop() on a
      // plugin whose start() rejected (closing a connection never opened) raises
      // a secondary error that masks the real startup failure.
      startedPlugins = startEntries.filter(({ plugin }) => startedPluginSet.has(plugin));
      const [firstFailure, ...extraFailures] = startResults.filter((result) => result.status === 'rejected');
      if (firstFailure) {
        // Starters run concurrently, so several can reject in one pass. The
        // first is thrown because callers already catch a single error and it is
        // normally the cause; the rest would otherwise be discarded by
        // Promise.allSettled, so log them — a sibling failure nobody reported is
        // the one that gets diagnosed twice.
        for (const extra of extraFailures) {
          const detail = extra.reason instanceof Error ? extra.reason.message : String(extra.reason);
          logger.error(`‼️ additional plugin start failure: ${detail}`, extra.reason);
        }
        throw firstFailure.reason;
      }
      logger.info(`🤖 ready`, { durationMs: Date.now() - startedAt });

      // Install global exception handlers so uncaught errors include context
      this._removeExceptionHandler = installExceptionHandler(logger);

      // Install signal handlers for graceful shutdown
      this._removeSignalHandlers = installSignalHandlers(() => this.stop(), logger);

      this.running = true;

      // Execute the runner if defined
      if (this.runner) {
        await this.runner();
      }
    } catch (error) {
      const detail = error instanceof Error ? error.message : String(error);
      logger.error(`‼️ startup failed: ${detail}`, error);
      // Tag the error so a downstream bootstrap guard (bootstrapServe) does not
      // log this same failure a second time when it catches the re-throw.
      markStartupFailureLogged(error);
      await this.cleanupAfterFailedStart(logger, { contextStarted, startedPlugins });
      // Re-throw so embedders, tests, and the serve entrypoint can handle the
      // failure. A library must not terminate its host process.
      throw error;
    }
  }

  /**
   * Graceful shutdown:
   * 1. Run shutdown hooks in reverse order (from all modules)
   * 2. Stop plugins in reverse tree order
   * 3. Close the DI container context
   */
  async stop(): Promise<void> {
    if (!this.running) {
      return;
    }

    const logger = useLogger('putnami');
    logger.debug('🛑 Shutting down...');

    // Remove signal and exception handlers
    this._removeSignalHandlers?.();
    this._removeSignalHandlers = undefined;
    this._removeExceptionHandler?.();
    this._removeExceptionHandler = undefined;

    // Run shutdown hooks (reverse), then stop plugins (reverse tree order).
    await runShutdownHooks(this.collectShutdownHooks(), logger);
    await stopPlugins(this.collectPlugins(), logger);

    // Close the DI container context
    if (this._context) {
      await closeContainerContext(this._context, logger);
      this._context = undefined;
    }

    this.running = false;
    logger.info('👋 Application stopped');
  }

  /**
   * Returns the active DI container context, or undefined if not built/started.
   * Unlike the `context` getter, this does NOT lazily build a new context.
   *
   * @internal Used by framework plugins (HTTP, events) that need DI scope access.
   */
  getActiveContext(): ContainerContext | undefined {
    return this._context;
  }

  /**
   * Returns a debug-friendly description of the DI container tree.
   * Useful for diagnosing dependency injection issues.
   */
  describeContainer(): ContainerDescription | undefined {
    return this._context?.describe();
  }

  private async cleanupAfterFailedStart(
    logger: ReturnType<typeof useLogger>,
    {
      contextStarted,
      startedPlugins,
    }: {
      contextStarted: boolean;
      startedPlugins: Array<{ plugin: Plugin; owner: Module }>;
    },
  ): Promise<void> {
    if (this.running) {
      await this.stop();
      return;
    }

    this._removeSignalHandlers?.();
    this._removeSignalHandlers = undefined;
    this._removeExceptionHandler?.();
    this._removeExceptionHandler = undefined;

    if (startedPlugins.length > 0) {
      await runShutdownHooks(this.collectShutdownHooks(), logger);
      await stopPlugins(startedPlugins, logger);
    }

    if (contextStarted && this._context) {
      await closeContainerContext(this._context, logger);
      this._context = undefined;
    }
  }

  /**
   * Checks if this application or any of its modules have DI registrations.
   */
  private hasRegistrations(): boolean {
    if (this.getRegistrations().length > 0) return true;
    if (getRegisteredConfigDefinitions().length > 0) return true;
    const modules = this.collectContainerModules();
    return modules.some((mod) => mod.getRegistrations().length > 0);
  }
}

/**
 * Creates a new Application.
 *
 * Factory function for fluent composition, consistent with `module()`.
 *
 * @returns A new Application instance
 *
 * @example
 * ```typescript
 * export const app = () =>
 *   application()
 *     .use(http({ port: 3000 }))
 *     .use(api())
 *     .use(staticFiles());
 * ```
 */
export function application(options?: ContainerContextOptions): Application {
  return new Application(options);
}
