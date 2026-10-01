import type {
  DesignBuilder,
  DesignContributor,
  DesignProvenance,
  GenerateResult,
  HealthChecker,
  InfraContributor,
  Module,
  Plugin,
  SourceScopedDesignContributor,
} from '@putnami/application';
import { designDataSchemaNode, designServiceNode, loadRegisteredModule } from '@putnami/application';
import type { MigrationRegistry } from '@putnami/migration';
import { tokenName, useLogger } from '@putnami/runtime';
import { robustRemove, robustRename } from '@putnami/runtime/robustio';
import {
  GeneratorHelper,
  getCurrentProject,
  getProjectRoot,
  joinPath,
  listProjectDependencies,
  relativePath,
} from '@putnami/utils';
import { randomUUID } from 'node:crypto';
import { mkdir, rm } from 'node:fs/promises';
import { pingDatabase } from './factory';
import { buildInfraRequirements, collectTableDefinitions, DATABASE_ENGINE, type InfraRequirements } from './infra';
import { SQLRunner } from './migrations';
import { normalizeDatasource, type PrimaryDatasource, setPrimaryDatasource } from './primary-datasource';
import { declaredRepositoryToken } from './repository/repository-di';
import type { TableDefinition } from './table';
import { UnitOfWork } from './unit-of-work';

export interface SqlPluginConfig {
  /**
   * AutoApply gates the automatic Migrate lifecycle phase. Default false:
   * the SQL runner no-ops unless the migrate CLI passes force=true.
   * Set true for samples and local dev where applying on startup is
   * convenient; leave false in production services that drive migrations
   * via the dedicated `migrate` binary.
   */
  autoApply?: boolean;
  /**
   * Pre-loaded module to use instead of dynamic import in warmup.
   * Pass the already-imported generated `.sql.gen.ts` module
   * to enable bundled builds where dynamic imports cannot be resolved.
   *
   * @example
   * ```typescript
   * import * as sqlModule from './.gen/src/.sql.gen.ts';
   * app.use(sql({ preloadedModule: sqlModule }));
   * ```
   */
  preloadedModule?: Record<string, unknown>;
  /**
   * The workload's primary (default) datasource. Names the logical datasource
   * the `/healthz` probe pings and that any `Table()` without an explicit `db`
   * inherits — for reads/writes and for the emitted infra requirements. The
   * bare-name shorthand (`'identity'`) or the explicit `{ name, schema }` form
   * are both accepted; `schema` is applied as the primary connection's
   * `search_path` in local dev (a deploy `DATABASE_BINDINGS` schema always
   * wins). Omitting it keeps the `default` behaviour. Mirrors the Go adapter's
   * `database.NewPlugin(PluginConfig{Datasource: ...})`.
   *
   * @example
   * ```typescript
   * app.use(sql({ datasource: 'identity' }));
   * app.use(sql({ datasource: { name: 'identity', schema: 'identity_auth' } }));
   * ```
   */
  datasource?: string | { name: string; schema?: string };
  /**
   * Opt into a DI request-scoped {@link UnitOfWork}. When true, `sql()` registers
   * a `scoped` `UnitOfWork` provider; pair it with `UnitOfWorkMiddleware()` on the
   * HTTP plugin so every request that touches a repository runs in one
   * transaction per participating datasource — committed on handler success,
   * rolled back on thrown error / AbortSignal. Off by default so existing
   * single-query/autocommit workloads keep their behaviour unchanged; opting in
   * makes each request that writes transactional. A `new`'d repository still
   * joins the same unit through the ambient transaction.
   *
   * Mirrors the Go adapter's `database.PluginConfig.UnitOfWork`.
   */
  unitOfWork?: boolean;
  /**
   * Timeout (ms) applied to the request-scoped {@link UnitOfWork}'s transaction
   * when `unitOfWork` is true, so a wedged handler cannot pin a pooled connection
   * indefinitely. 0 / omitted disables the bound. Ignored unless `unitOfWork` is
   * true. Mirrors the Go adapter's `PluginConfig.UnitOfWorkTimeout`.
   */
  unitOfWorkTimeoutMs?: number;
}

/**
 * SQL plugin for the Putnami framework.
 *
 * - Resolves the per-app `MigrationRegistry` from DI during warmup.
 * - Constructs an `SQLRunner` and registers it for `kind = "sql"`.
 * - Loads the generated `.sql.gen.ts` (when present) so feature plugins
 *   that publish `MigrationSource` values get a chance to do so before
 *   the framework's Migrate lifecycle phase runs.
 *
 * The runner itself doesn't apply migrations from `warmup` — the
 * framework's Migrate phase calls `registry.applyAll()` after every
 * plugin's warmup completes. The migrate CLI bypasses Migrate
 * entirely and dispatches against the registry directly.
 */
export function sql(
  config: SqlPluginConfig = {},
): Plugin &
  HealthChecker &
  DesignContributor &
  SourceScopedDesignContributor &
  InfraContributor & { readonly primaryDatasource: PrimaryDatasource | undefined } {
  // Resolve the declared primary datasource once. Used by the health probe
  // (which datasource to ping), warmup (the process-global the data path reads),
  // and generate (the infra fallback for tables that set no `db`).
  const primary: PrimaryDatasource | undefined = normalizeDatasource(config.datasource);
  let designTables: TableDefinition[] = [];

  return {
    /**
     * Probe identifier surfaced as the JSON key on /healthz. The
     * platform plugin auto-discovers this via the HealthChecker type
     * guard — no manual addHealthChecker() call needed.
     */
    name: 'database',

    /**
     * The declared primary datasource and its owning schema. A library that
     * contributes tables to the same datasource reads it to put them in the
     * schema this workload's connections search.
     */
    primaryDatasource: primary,

    /** Project native datasource declarations through the bounded infra seam. */
    designInfraRequirements() {
      const requirements = new Map<string, DesignProvenance[]>();
      if (primary?.name) requirements.set(primary.name, []);
      for (const table of designTables) {
        const name = table.options.db || primary?.name || 'default';
        const sources = requirements.get(name) ?? [];
        if (table.__source && !sources.some(({ path }) => path === table.__source?.path)) sources.push(table.__source);
        requirements.set(name, sources);
      }
      return [...requirements]
        .sort(([left], [right]) => left.localeCompare(right))
        .map(([name, sources]) => ({
          name,
          kind: 'database' as const,
          ...(sources.length > 0
            ? { sources: sources.sort((left, right) => left.path.localeCompare(right.path)) }
            : {}),
        }));
    },

    /**
     * /healthz probe: pings the workload's primary datasource via `SELECT 1`
     * (the `default` datasource when none was declared). The platform plugin's
     * per-probe timeout caps how long a hung query can stall the response.
     */
    async checkHealth(): Promise<void> {
      await pingDatabase(primary?.name);
    },

    async generate(_: Module): Promise<GenerateResult> {
      const project = getCurrentProject();
      const projectPath = getProjectRoot();

      // Find dependency packages that use @putnami/database
      const dependencies = listProjectDependencies(project.name);
      const sqlDependencies: string[] = [];
      for (const dependency of dependencies) {
        if (dependency === project.name || dependency === '@putnami/database') {
          continue;
        }
        const dep = listProjectDependencies(dependency);
        if (dep?.includes('@putnami/database')) {
          sqlDependencies.push(dependency);
        }
      }

      // Find local table files that import from @putnami/database
      const localTableFiles: string[] = [];
      const glob = new Bun.Glob('src/**/*.ts');
      for (const file of glob.scanSync({ cwd: projectPath, absolute: false })) {
        const content = await Bun.file(joinPath(projectPath, file)).text();
        if (content.includes('@putnami/database') && /\bTable\s*\(/.test(content)) {
          localTableFiles.push(file);
        }
      }

      // Emit (or clear) this project's infra scratch fragment. Runs before the
      // early return so a fragment from a prior generate is removed once the
      // project declares no tables. Tables are folded in from both the
      // project's local files and its @putnami/database-using dependencies: a
      // thin workload commonly keeps every Table() in a library, and such a
      // library never runs this generate() hook, so it emits no fragment of its
      // own. The workload must therefore claim the databases its tables need —
      // mirroring the Go plugin, whose Describe() collects the dependency
      // graph's migration sources.
      designTables = await emitInfraRequirements(
        projectPath,
        project.name,
        localTableFiles,
        sqlDependencies,
        primary?.name,
      );

      if (sqlDependencies.length === 0 && localTableFiles.length === 0) {
        return {};
      }

      const loaderPath = joinPath(projectPath, '.gen/src/.sql.gen.ts');
      const loaderDir = joinPath(projectPath, '.gen/src');
      const sqlGenerator = new GeneratorHelper(loaderPath);
      sqlGenerator.appendHead(`// generated by the SqlPlugin exploring '${project.name}'`);

      for (const dependency of sqlDependencies) {
        sqlGenerator.appendHead(`import "${dependency}";`);
      }
      for (const file of localTableFiles) {
        const rel = relativePath(loaderDir, joinPath(projectPath, file));
        sqlGenerator.appendHead(`import "${sqlGenerator.normalizeImport(rel)}";`);
      }

      sqlGenerator.write();

      return {
        exports: {
          'sql-loader': loaderPath,
        },
      };
    },
    /**
     * Each table locates its own declaration, so this plugin is attributed by
     * source containment rather than by the module it is registered on. `sql()`
     * is an application-wide plugin — one runner, one primary datasource — and
     * the documented composition registers it on the application root, which
     * declares no feature. Without this the contributor would only ever run for
     * an app that nested `sql()` inside a feature module, silently omitting
     * every table and repository relationship from the normal setup.
     */
    designAttribution: 'source',
    /**
     * Project the native data declarations this project captured during
     * `generate()`. Only declarations are projected: a table exists because a
     * `Table()` names it, a namespace because a `Table()` or a migration source
     * declares it, and a repository because a native declaration minted its DI
     * token. A migration's SQL text is never read, so a relation that only some
     * `CREATE TABLE` statement mentions stays unrepresented rather than being
     * guessed into the graph.
     */
    contributeDesign(builder: DesignBuilder): void {
      for (const table of designTables) {
        const datasource = table.options.db || primary?.name || 'default';
        // `data.table`, not `data.schema`: a table is one relation inside a
        // datasource namespace, and sharing the schema kind would let a table
        // named after a schema on the same datasource mint a colliding node ID.
        //
        // The declared namespace is part of the identity: a relation is only
        // unique within its schema, so two `Table()` calls that share a
        // datasource and a name but declare different schemas are two
        // relations.
        const tableId = `data.table:${datasource}${table.options.schema ? `:${table.options.schema}` : ''}:${table.tableName}`;
        builder.addNode({
          id: tableId,
          kind: 'data.table',
          name: table.tableName,
          properties: {
            columns: Object.keys(table.schema).sort().join(','),
            datasource,
            ...(table.options.schema ? { schema: table.options.schema } : {}),
          },
          ...(table.__source ? { provenance: table.__source } : {}),
        });
        if (table.__source?.path) builder.relateFromSource(table.__source.path, tableId, 'contains');

        // Only a declared schema mints a namespace node. A table with no
        // `schema` inherits whatever `search_path` the deployment binds, which
        // is a deploy fact rather than a declaration — and inventing a node for
        // it would leave an unreachable namespace in every graph. A declared one
        // is normally also declared by the migration source that provisions it,
        // and the shared node shape is what keeps those two declarations from
        // conflicting.
        if (table.options.schema) {
          const schema = designDataSchemaNode({ datasource, engine: DATABASE_ENGINE, schema: table.options.schema });
          builder.addNode(schema);
          builder.addEdge({ from: schema.id, to: tableId, kind: 'contains', authority: 'exact' });
        }

        // A repository is a declaration, not a property of the table: the token
        // exists only because `provideRepository(T)` or a `repositoryToken(T)`
        // injection named one. Mint the same service identity every other
        // producer mints for that token, so an endpoint injecting the repository
        // and this projection describe one node.
        const token = declaredRepositoryToken(table);
        if (!token) continue;
        const repository = designServiceNode(tokenName(token));
        builder.addNode(repository);
        // `Repository<T>` is declared over exactly one table and its API is
        // read-write in both directions; the declaration grants both, so both
        // edges are exact. Narrowing them to the methods a caller happens to
        // reach would mean reading call sites, which is source-text inference.
        builder.addEdge({ from: repository.id, to: tableId, kind: 'reads', authority: 'exact' });
        builder.addEdge({ from: repository.id, to: tableId, kind: 'writes', authority: 'exact' });
      }
    },
    async warmup(owner: Module) {
      const logger = useLogger('sql');

      // Publish the declared primary datasource process-globally so the data
      // path (Repository.conn / useConfig) resolves a `Table()` with no `db` to
      // it. One application per process (serverless-first), so a single primary
      // is unambiguous. Set before any request handling, which runs after warmup.
      setPrimaryDatasource(primary);

      // Opt-in DI request-scoped UnitOfWork. Registered during warmup — the
      // Application rebuilds the DI context after every warmup, so this scoped
      // provider is picked up. A fresh UnitOfWork materializes per request scope
      // and opens no connection until a repository writes; `UnitOfWorkMiddleware`
      // drives its commit/rollback at the request boundary.
      if (config.unitOfWork) {
        owner.provide(UnitOfWork, () => new UnitOfWork({ timeoutMs: config.unitOfWorkTimeoutMs }), { scope: 'scoped' });
      }

      // Resolve the per-app MigrationRegistry from the application root.
      // Warmup runs before the DI container is built, so `context.get` is not
      // available; the Application exposes its registry via
      // getMigrationRegistry().
      const registry = resolveMigrationRegistry(owner);

      // Load the generated `.sql.gen.ts` so Table() side-effects run
      // before contributors are collected. Plugins that ship migrations
      // via @putnami/database's MigrationContributor get their
      // sources added by the framework right after this hook returns.
      if (config.preloadedModule) {
        logger.debug('SQL tables registered (preloaded)');
      } else {
        const registered = await loadRegisteredModule('sql-loader');
        if (registered) {
          logger.debug('SQL tables registered (bundled)');
        } else {
          const project = getCurrentProject();
          if (project) {
            const sqlGeneratedPath = joinPath(project.path, '.gen/src/.sql.gen.ts');
            if (await Bun.file(sqlGeneratedPath).exists()) {
              await import(sqlGeneratedPath);
            }
          }
        }
      }

      // Register the SQL runner. The framework's Migrate phase calls
      // runner.apply() after this warmup returns and after every
      // MigrationContributor has been collected; non-forced apply
      // respects autoApply.
      registry.registerRunner(new SQLRunner({ registry, autoApply: config.autoApply ?? false }));
    },
  };
}

/**
 * Per-producer scratch slug. Each framework producer owns one file at
 * `<project>/.gen/infra/<slug>.json`; the TypeScript generator syncs all
 * fragments into committed `infra/requirements.json`.
 */
const SIDECAR_SLUG = 'database';

/** Per-producer infra scratch-fragment path, relative to the project root. */
function infraSidecarPath(projectPath: string): string {
  return joinPath(projectPath, `.gen/infra/${SIDECAR_SLUG}.json`);
}

/**
 * Derive the `infra.databases` the project's tables require and write the
 * database producer's scratch fragment at `.gen/infra/database.json`. Table definitions
 * are materialised by importing the project's local table files and each
 * `@putnami/database`-using dependency package, then scanning their exports —
 * so a workload whose tables live entirely in a library still emits the
 * fragment.
 *
 * When no pools are declared — including the case where the last table was just
 * removed — any scratch fragment from a prior generate is deleted so generator
 * sync can't keep reading a requirement that no longer exists.
 */
async function emitInfraRequirements(
  projectPath: string,
  projectName: string,
  localTableFiles: readonly string[],
  sqlDependencies: readonly string[],
  defaultDatasource?: string,
): Promise<TableDefinition[]> {
  const tables: TableDefinition[] = [];
  for (const file of localTableFiles) {
    const module = (await import(joinPath(projectPath, file))) as Record<string, unknown>;
    tables.push(...collectTableDefinitions(module));
  }
  for (const dependency of sqlDependencies) {
    // Resolve the dependency from the consuming workload root before importing.
    // A bare `import(dependency)` resolves relative to this plugin's own module
    // location, deep inside Bun's isolated `.bun` store — but under the isolated
    // node_modules linker a `workspace:*` lib is only symlinked into the trees of
    // packages that directly depend on it (the workload), and isolated mode does
    // not hoist it to the repo root. Resolving from projectPath (where the lib is
    // linked) mirrors the local-file branch above.
    const resolved = Bun.resolveSync(dependency, projectPath);
    const module = (await import(resolved)) as Record<string, unknown>;
    tables.push(...collectTableDefinitions(module));
  }

  const requirements = buildInfraRequirements(tables, defaultDatasource);
  if (!requirements) {
    await robustRemove(infraSidecarPath(projectPath));
    return tables;
  }

  useLogger('sql').debug(`emitting infra requirements for '${projectName}'`);
  await writeInfraRequirements(projectPath, requirements);
  return tables;
}

/**
 * Write the per-project infra manifest atomically (unique temp file + rename).
 * The same generate task runs for both the build and test pipelines and may run
 * concurrently, while the generator sync reads scratch fragments — a per-write
 * temp name keeps concurrent writers from clobbering each other's temp file,
 * and the rename guarantees readers observe a complete file.
 */
async function writeInfraRequirements(projectPath: string, requirements: InfraRequirements): Promise<void> {
  const dir = joinPath(projectPath, '.gen/infra');
  await mkdir(dir, { recursive: true });
  const finalPath = infraSidecarPath(projectPath);
  const tmpPath = `${finalPath}.${randomUUID()}.tmp`;
  await Bun.write(tmpPath, `${JSON.stringify(requirements, null, 2)}\n`);
  try {
    await robustRename(tmpPath, finalPath);
  } catch (err) {
    // Best-effort cleanup: it must not replace the write error.
    await rm(tmpPath, { force: true }).catch(() => undefined);
    throw err;
  }
}

function resolveMigrationRegistry(owner: Module): MigrationRegistry {
  const root = owner.getRoot() as Module & { getMigrationRegistry?: () => MigrationRegistry };
  if (typeof root.getMigrationRegistry === 'function') {
    return root.getMigrationRegistry();
  }
  throw new Error(
    'SQL plugin could not reach the application MigrationRegistry; ensure the plugin is .use()d under an Application',
  );
}
