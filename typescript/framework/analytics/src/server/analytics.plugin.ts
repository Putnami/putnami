import { applyPrefix, type GenerateResult, HttpPlugin, type Module, type Plugin } from '@putnami/application';
import type { SQLSource } from '@putnami/database';
import { type ConfigDefinition, type Logger, useConfig, useLogger } from '@putnami/runtime';
import { AnalyticsConfig, type AnalyticsConfigValues, type AnalyticsOptions, resolveSecret } from './analytics.config';
import { resolveAnalyticsDatasource } from './datasource';
import { registerDeclared } from './declare';
import { INGEST_PATH, registerIngestRoute } from './http/ingest.route';
import { loadKnownRoutes } from './http/known-routes';
import { pageViewMiddleware } from './http/page-view.middleware';
import { registerTrackerAsset } from './http/tracker-asset';
import { type AnalyticsRuntime, createRuntime, setAnalyticsRuntime } from './runtime';
import { createDedupCache } from './sink/dedup-cache';
import { createMigrationSource, DEFAULT_SCHEMA, isAnalyticsSchema } from './sink/migrations';
import { createNoopSink } from './sink/noop';
import type { WriteQueue } from './sink/queue';
import { createSink, type Sink } from './sink/sink';

/**
 * `sql()` is a factory returning an object literal, not a class, so there is
 * no constructor to hand `findPlugin`. The probe name is the plugin's own
 * `name` — the identifier `/healthz` already keys its probe on — narrowed by
 * one member unique to the SQL plugin's contributor surface.
 */
const SQL_PLUGIN_NAME = 'database';

/** What decides where the tables go, once the workload is known to own them. */
interface Contribution {
  owner: Module;
  datasource: string;
  config: AnalyticsConfigValues;
  /** The schema the last migration source was built in; the sink writes there. */
  schema?: string;
}

/** Logged once when the application composes no database. */
export const NO_SQL_WARNING = 'analytics: no sql() plugin — events are counted in metrics only and not stored';

/**
 * The analytics collection plugin.
 *
 * Composing it is the opt-in: there is no default egress, no third party, and
 * no consent banner in the default `cookieless` mode. What it does at warmup
 * is resolve every decision once — the secret, the datasource, the declared
 * vocabulary, the known routes — and then register exactly two things on the
 * HTTP plugin: the global page-view middleware and the ingest route.
 *
 * It implements the migration contributor shape structurally, so `sql()` plus
 * the framework's Migrate phase create the tables. An application without a
 * database still composes: it gets a counting no-op sink and no migration.
 */
export class AnalyticsPlugin implements Plugin {
  readonly name = '@putnami/analytics';

  private contribution: Contribution | undefined;

  /** Set while the schema is resolved, so plugins reading each other's sources terminate. */
  private resolving = false;

  private runtime: AnalyticsRuntime | undefined;

  private ticker: (() => void) | undefined;

  constructor(private readonly options: AnalyticsOptions = {}) {}

  /**
   * Publishes the `analytics` config block through composition.
   *
   * Config extraction walks the composed tree, so this is what puts the
   * package's keys in the workload's `schema/config.json` — and its one
   * `Sensitive` field in `.gen/infra/secrets.json`, which the generator folds
   * into `infra/requirements.json` as `analytics.secret`. Without it a
   * deployment has no way to know the plugin needs a key, and the workload
   * that composed analytics fails closed at warmup for a requirement it never
   * declared.
   *
   * @returns The one config definition this package owns.
   */
  configDefinitions(): ConfigDefinition[] {
    return [AnalyticsConfig];
  }

  /**
   * Contributes the analytics tables to the build, not only to the runtime.
   *
   * `Application.build()` emits the workload's infra requirement
   * (`.gen/infra/migration.json`, which the generator syncs into the committed
   * `infra/requirements.json`) and its migration bundle from every
   * `MigrationContributor`'s sources, **right after generate and long before
   * warmup**. A plugin that only fills `migrationSources()` at warmup is
   * therefore absent from both: a deployment reads a manifest that asks for no
   * database and a bundle that creates no table, and the tables only ever
   * appear under `sql({ autoApply: true })`.
   *
   * Two things differ from warmup on purpose, because a build artifact must
   * not depend on the environment the build ran in:
   *
   * - `enabled` is not consulted. Composing the plugin is what makes the
   *   workload own the four tables; `analytics.enabled: false` is a runtime
   *   switch that stops collection, not a way to un-declare infrastructure.
   * - The datasource is the **configured** name, not the resolved one.
   *   Resolution falls back to `default` when the name is declared in no
   *   binding — and at build time there is no binding, because this manifest
   *   is what asks for one.
   *
   * The source itself is built when it is read, so its schema follows what
   * every other plugin contributes, whatever order they generate in.
   *
   * @param owner - The module the plugin was composed on.
   * @returns Nothing; the sources are read by the caller.
   */
  async generate(owner: Module): Promise<GenerateResult> {
    const config = useConfig(AnalyticsConfig, { confInit: configInit(this.options) });
    this.contribution = hasSqlPlugin(owner) ? { owner, datasource: config.datasource, config } : undefined;
    return {};
  }

  /**
   * Resolves every decision and wires the HTTP seams.
   *
   * Two conditions fail closed here rather than degrading silently: a missing
   * secret (the visitor hash has no key, so there is no anonymization to
   * speak of) and `mode: 'identified'` without a consent callback (a
   * persistent cookie nobody gated). Both are configuration mistakes an
   * operator must see at startup, not a request at a time.
   *
   * @param owner - The module the plugin was composed on.
   */
  async warmup(owner: Module): Promise<void> {
    const config = useConfig(AnalyticsConfig, { confInit: configInit(this.options) });
    if (!config.enabled) {
      return;
    }
    const logger = useLogger('@putnami/analytics');
    const secret = resolveSecret(config);
    if (config.mode === 'identified' && !this.options.consent) {
      throw new Error('analytics: mode "identified" requires a consent(ctx) callback');
    }
    const datasource = resolveAnalyticsDatasource(config.datasource);
    logger.info(`analytics: datasource "${datasource}", mode ${config.mode}`);

    const declared = registerDeclared(this.options.events ?? {});
    const httpPlugin = await owner.ensurePlugin(HttpPlugin);
    const trackerUrl = registerTrackerAsset(httpPlugin, config);
    const dedup = createDedupCache();
    const runtime = createRuntime({
      config,
      secret,
      datasource,
      declared: declared.names,
      declaredSchemas: declared.schemas,
      trackerUrl,
      knownRoutes: loadKnownRoutes(),
      // A module base path moves the route; the bootstrap has to name the
      // moved one or the browser POSTs into a 404.
      endpoint: applyPrefix(INGEST_PATH, owner.getPath()),
      sink: this.resolveSink(owner, config, datasource, logger, dedup),
      dedup,
      logger,
      consent: this.options.consent,
    });

    this.runtime = runtime;
    setAnalyticsRuntime(runtime);
    this.ticker = startFlushTicker(runtime.queue, config.flushIntervalMs);
    httpPlugin.use(pageViewMiddleware(runtime));
    registerIngestRoute(httpPlugin, runtime);
  }

  /**
   * The migration sources the framework collects between warmup and migrate.
   *
   * Structurally a `MigrationContributor`: the framework discovers it with a
   * runtime type guard, so the package does not have to depend on
   * `@putnami/migration` to contribute.
   *
   * @returns The analytics tables, or nothing when no database is composed.
   */
  migrationSources(): SQLSource[] {
    const contribution = this.contribution;
    if (!contribution || this.resolving) {
      return [];
    }
    contribution.schema = this.schemaOf(contribution);
    return [createMigrationSource(contribution.datasource, contribution.config, contribution.schema)];
  }

  /**
   * Writes what the queue is still holding, then stops answering.
   *
   * This is the one place analytics is allowed to wait for the database, and
   * the only reason it is allowed: SIGTERM reaches here through
   * `installSignalHandlers`, which force-exits the process after
   * `SHUTDOWN_TIMEOUT_MS` (10 000 ms). `flushDeadlineMs` defaults to half of
   * that, so a database that never answers costs a bounded pause and the rows
   * it was holding — never a shutdown that hangs until SIGKILL.
   *
   * @param _owner - The owning module; unused.
   */
  async stop(_owner: Module): Promise<void> {
    // Before the drain: a tick landing mid-drain would take the one in-flight
    // slot and make the deadline harder to honour.
    this.ticker?.();
    this.ticker = undefined;
    const runtime = this.runtime;
    if (runtime) {
      setAnalyticsRuntime(undefined);
      this.runtime = undefined;
      await runtime.queue.drain(runtime.config.flushDeadlineMs);
    }
  }

  /** Picks the Postgres sink or the no-op one, and contributes accordingly. */
  private resolveSink(
    owner: Module,
    config: AnalyticsConfigValues,
    datasource: string,
    logger: Logger,
    dedup: ReturnType<typeof createDedupCache>,
  ): Sink {
    if (this.options.__sink) {
      return this.options.__sink;
    }
    if (!hasSqlPlugin(owner)) {
      // Never throw: the measurement is optional, the application is not.
      logger.warn(NO_SQL_WARNING);
      return createNoopSink();
    }
    const contribution: Contribution = { owner, datasource, config };
    this.contribution = contribution;
    return createSink({
      datasource,
      // The schema the migration source was built in, so the writes and the
      // migration cannot disagree; resolved here only when no source was read.
      schema: () => contribution.schema ?? this.schemaOf(contribution),
      config,
      logger,
      dedup,
      now: () => new Date(),
      connect: this.options.__connect,
    });
  }

  /** Resolves the tables' schema against what the tree contributes now. */
  private schemaOf(contribution: Contribution): string {
    this.resolving = true;
    try {
      return resolveAnalyticsSchema(contribution.owner, contribution.datasource, contribution.config.schema);
    } finally {
      this.resolving = false;
    }
  }
}

/**
 * Starts the unref-ed ticker that offers the queue a flush on a schedule.
 *
 * The request-based CPU story is the whole reason it is worth having *and* the
 * reason it cannot be the only trigger: between requests the callback is
 * throttled and simply runs at the next CPU window, which the platform's
 * keep-warm ping provides. So the ticker is never worse than waiting for a
 * request, it covers routes that bypass the middleware chain, and on
 * always-allocated CPU it flushes on schedule. It is unref-ed so it never by
 * itself keeps a process alive.
 *
 * @param queue - The queue to kick.
 * @param intervalMs - The period, the same value the elected request uses.
 * @returns A function that stops the ticker.
 */
export function startFlushTicker(queue: WriteQueue, intervalMs: number): () => void {
  const handle = setInterval(
    () => {
      queue.kick();
    },
    Math.max(1, intervalMs),
  );
  if (handle && typeof handle === 'object' && 'unref' in handle) {
    (handle as { unref: () => void }).unref();
  }
  return () => clearInterval(handle);
}

/**
 * Composes analytics collection into an application.
 *
 * @param options - Configuration overrides plus the two code-only members.
 * @returns The plugin.
 */
export const analytics = (options: AnalyticsOptions = {}): AnalyticsPlugin => new AnalyticsPlugin(options);

/**
 * Reports whether the application composed a SQL plugin.
 *
 * The whole module tree is walked, not just the ancestors, because `sql()` and
 * `analytics()` are routinely registered as siblings on the application root
 * and warmup order between them is not guaranteed.
 *
 * @param owner - The module the plugin was composed on.
 * @returns True when a `sql()` plugin is anywhere in the tree.
 */
export function hasSqlPlugin(owner: Module): boolean {
  return findSqlPlugin(owner) !== undefined;
}

/**
 * Resolves the schema the analytics tables live in on `datasource`.
 *
 * The runner refuses one datasource in two schemas, so the tables follow the
 * datasource rather than impose `public` on it. In order:
 *
 * 1. `analytics.schema`, when set;
 * 2. the schema another SQL migration source declares for the same datasource;
 * 3. the schema `sql({ datasource: { name, schema } })` declares, when that
 *    primary datasource is this one;
 * 4. `public`.
 *
 * An inherited schema is adopted only when it is a single lowercase name the
 * tables can live in. A `search_path` list such as `app, public`, or a
 * mixed-case name, falls through to `public`, where the tables have always
 * been.
 *
 * @param owner - The module the plugin was composed on.
 * @param datasource - The datasource the tables go to.
 * @param configured - The `analytics.schema` value.
 * @returns The schema name.
 */
export function resolveAnalyticsSchema(owner: Module, datasource: string, configured: string | undefined): string {
  const explicit = configured?.trim();
  if (explicit) {
    return explicit;
  }
  // This plugin's own sources are empty while it resolves, so it never reads
  // its own schema back.
  for (const { plugin } of owner.getRoot().collectPlugins()) {
    const contributor = plugin as Partial<{ migrationSources(): unknown[] }>;
    if (typeof contributor.migrationSources !== 'function') {
      continue;
    }
    for (const source of contributor.migrationSources()) {
      const declared = sqlSourceSchema(source, datasource);
      if (declared) {
        return adoptable(declared);
      }
    }
  }
  const primary = (findSqlPlugin(owner) as Partial<{ primaryDatasource: { name: string; schema?: string } }>)
    ?.primaryDatasource;
  if (primary?.name === datasource && primary.schema) {
    return adoptable(primary.schema);
  }
  return DEFAULT_SCHEMA;
}

/** An inherited schema, or `public` when the tables cannot live in it. */
function adoptable(schema: string): string {
  return isAnalyticsSchema(schema) ? schema : DEFAULT_SCHEMA;
}

/** The schema a SQL migration source declares for `datasource`, if it is one that does. */
function sqlSourceSchema(source: unknown, datasource: string): string | undefined {
  const candidate = source as Partial<Pick<SQLSource, 'kind' | 'datasource' | 'schema'>> | null;
  if (candidate?.kind !== 'sql' || candidate.datasource !== datasource) {
    return undefined;
  }
  return candidate.schema || undefined;
}

/** The `sql()` plugin anywhere in the module tree, if one is composed. */
function findSqlPlugin(owner: Module): Plugin | undefined {
  return owner
    .getRoot()
    .collectPlugins()
    .find(
      ({ plugin }) =>
        plugin.name === SQL_PLUGIN_NAME &&
        typeof (plugin as Record<string, unknown>)['designInfraRequirements'] === 'function',
    )?.plugin;
}

/** Strips the code-only members so the config loader only sees config. */
function configInit(options: AnalyticsOptions): Partial<AnalyticsConfigValues> {
  const { consent: _consent, events: _events, __sink: _sink, __connect: _connect, ...values } = options;
  return values;
}
