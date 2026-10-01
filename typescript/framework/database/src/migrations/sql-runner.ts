import {
  type ApplyOpts,
  DatasourceSchemaConflictError,
  type DriftReport,
  type Kind,
  KindSQL,
  type MigrationRecord,
  type MigrationRegistry,
  type MigrationRunner,
  type RollbackOpts,
  resolveDatasourceSchemas,
  sha256Hex,
} from '@putnami/migration';
import { useLogger } from '@putnami/runtime';
import { MigrationError } from '../errors';
import { DATABASE_MIGRATION_LOGGER, migrationFields } from '../observability/database-logging';
import type { Migration } from './migration.entity';
import { Migrator } from './migrator';
import { type SQLDefinition, SQLSource } from './sql-source';

export interface SQLRunnerOptions {
  registry: MigrationRegistry;
  /**
   * AutoApply gates the automatic Migrate lifecycle phase. Default false:
   * runners no-op unless ApplyOpts.force is set (which the migrate CLI
   * passes on `up`). Set true for samples and local dev.
   */
  autoApply?: boolean;
}

/**
 * MigrationRunner implementation for `kind = "sql"`. Multi-datasource
 * fan-out: one Migrator per datasource discovered across the registry's
 * SQLSources. Materialization is lazy and cached after first use; a
 * late-arriving source is visible on the next call.
 */
export class SQLRunner implements MigrationRunner {
  readonly kind: Kind = KindSQL;
  private readonly autoApply: boolean;
  private readonly registry: MigrationRegistry;
  private readonly logger = useLogger(DATABASE_MIGRATION_LOGGER);
  private migrators: Map<string, Migrator> | undefined;

  constructor(opts: SQLRunnerOptions) {
    this.registry = opts.registry;
    this.autoApply = opts.autoApply ?? false;
  }

  async apply(opts: ApplyOpts = {}): Promise<MigrationRecord[]> {
    if (!opts.force && !this.autoApply) {
      this.logger.debug('SQL runner apply skipped (autoApply=false; pass force=true to override)');
      return [];
    }
    const migrators = this.materialize();
    const out: MigrationRecord[] = [];
    for (const datasource of [...migrators.keys()].sort()) {
      const mig = migrators.get(datasource)!;
      // biome-ignore lint/performance/noAwaitInLoops: serial across datasources
      const applied = opts.to ? await mig.upTo(opts.to) : await mig.up();
      out.push(...applied.map((row) => this.toRecord(row, 'applied', datasource)));
    }
    if (out.length > 0) {
      this.logger.info('migrations applied', migrationFields({ count: out.length, datasources: migrators.size }));
    } else {
      this.logger.debug('migrations up to date', migrationFields({ datasources: migrators.size }));
    }
    return out;
  }

  async status(): Promise<MigrationRecord[]> {
    const migrators = this.materialize();
    const out: MigrationRecord[] = [];
    for (const datasource of [...migrators.keys()].sort()) {
      const mig = migrators.get(datasource)!;
      // biome-ignore lint/performance/noAwaitInLoops: serial across datasources
      const rows = await mig.status();
      const appliedNames = new Set(rows.map((r) => r.name));
      for (const row of rows) {
        const def = mig.getDefinitions().find((d) => d.name === row.name);
        out.push(this.toRecord(row, row.success === 1 ? 'applied' : 'failed', datasource, def));
      }
      for (const def of mig.getDefinitions()) {
        if (appliedNames.has(def.name)) continue;
        out.push({
          kind: this.kind,
          namespace: def.namespace,
          name: def.name,
          status: 'pending',
          // biome-ignore lint/performance/noAwaitInLoops: serial across datasources
          hash: await sha256Hex(def.sql),
          source: def.source,
          target: datasource,
        });
      }
    }
    return out;
  }

  async rollback(opts: RollbackOpts = {}): Promise<MigrationRecord[]> {
    const migrators = this.materialize();
    const out: MigrationRecord[] = [];
    for (const datasource of [...migrators.keys()].sort()) {
      const mig = migrators.get(datasource)!;
      if (!opts.to) {
        // biome-ignore lint/performance/noAwaitInLoops: serial across datasources
        const row = await mig.rollback();
        if (row) out.push(this.toRecord(row, 'rolled-back', datasource));
        continue;
      }
      // biome-ignore lint/performance/noAwaitInLoops: serial across datasources
      const rolled = await mig.rollbackTo(opts.to);
      for (const row of rolled) {
        out.push(this.toRecord(row, 'rolled-back', datasource));
      }
    }
    if (out.length > 0) {
      this.logger.info('migrations rolled back', migrationFields({ count: out.length, datasources: migrators.size }));
    } else {
      this.logger.debug('no migrations rolled back', migrationFields({ datasources: migrators.size }));
    }
    return out;
  }

  async verify(): Promise<DriftReport> {
    const migrators = this.materialize();
    const report: DriftReport = {
      kind: this.kind,
      hashDrifts: [],
      missingFromRegistry: [],
      missingFromStore: [],
    };
    for (const datasource of [...migrators.keys()].sort()) {
      const mig = migrators.get(datasource)!;
      // biome-ignore lint/performance/noAwaitInLoops: serial across datasources
      const rows = await mig.status();
      const known = new Map(mig.getDefinitions().map((d) => [d.name, d]));
      const seen = new Set<string>();
      for (const row of rows) {
        if (row.success !== 1) continue;
        seen.add(row.name);
        const def = known.get(row.name);
        if (!def) {
          report.missingFromRegistry?.push(this.toRecord(row, 'applied', datasource));
          continue;
        }
        // biome-ignore lint/performance/noAwaitInLoops: serial check
        const currentHash = await sha256Hex(def.sql);
        if (currentHash !== row.hash) {
          report.hashDrifts?.push({
            namespace: def.namespace,
            name: def.name,
            storedHash: row.hash,
            currentHash,
            target: datasource,
          });
        }
      }
      for (const def of known.values()) {
        if (seen.has(def.name)) continue;
        // biome-ignore lint/performance/noAwaitInLoops: serial check
        const currentHash = await sha256Hex(def.sql);
        report.missingFromStore?.push({
          kind: this.kind,
          namespace: def.namespace,
          name: def.name,
          status: 'pending',
          hash: currentHash,
          source: def.source,
          target: datasource,
        });
      }
    }
    return report;
  }

  // --- internals ------------------------------------------------------------

  /**
   * Group contributed SQLSources by datasource, flatten their
   * definitions, detect (datasource, name) duplicates across sources,
   * and instantiate one Migrator per datasource.
   */
  private materialize(): Map<string, Migrator> {
    if (this.migrators) return this.migrators;

    const byDatasource = new Map<string, SQLDefinition[]>();
    const sources: SQLSource[] = [];
    for (const src of this.registry.sourcesFor(KindSQL)) {
      if (!(src instanceof SQLSource)) {
        throw new MigrationError(
          `non-SQLSource value contributed for kind=sql: ${src.constructor.name} (namespace=${src.namespace})`,
          'sql',
        );
      }
      sources.push(src);
      for (const def of src.definitions) {
        const ds = def.datasource ?? src.datasource;
        const list = byDatasource.get(ds) ?? [];
        list.push(def);
        byDatasource.set(ds, list);
      }
    }
    // Each datasource's owning schema becomes its Migrator's search_path so
    // unqualified DDL lands in it — letting one migration set target many
    // schemas via distinct Datasource{name, schema} pairs. The bundle generator
    // runs the same rule at build time.
    const schemaByDatasource = datasourceSchemas(sources);

    this.migrators = new Map();
    for (const [datasource, defs] of byDatasource) {
      const seen = new Map<string, string>();
      for (const d of defs) {
        const prior = seen.get(d.name);
        if (prior !== undefined) {
          throw new MigrationError(
            `duplicate migration "${d.name}" for datasource "${datasource}" (first source=${prior}, second=${d.source ?? '?'})`,
            d.name,
          );
        }
        seen.set(d.name, d.source ?? '');
      }
      this.migrators.set(datasource, new Migrator(datasource, defs, schemaByDatasource.get(datasource) ?? ''));
    }
    return this.migrators;
  }

  private toRecord(
    row: Migration,
    status: MigrationRecord['status'],
    target: string,
    def?: SQLDefinition,
  ): MigrationRecord {
    return {
      kind: this.kind,
      namespace: def?.namespace ?? namespaceFromName(row.name),
      name: row.name,
      status,
      hash: row.hash,
      downHash: row.downHash,
      executedAt: row.executedAt,
      durationMs: row.executionTimeMs,
      error: row.errorMessage,
      source: def?.source,
      target,
    };
  }
}

function namespaceFromName(name: string): string | undefined {
  const i = name.indexOf('/');
  return i > 0 ? name.slice(0, i) : undefined;
}

/**
 * Each datasource's owning schema across `sources`. Two sources mapping one
 * datasource to different schemas is a contradiction (ambiguous search path;
 * the shared state store cannot tell them apart), so it is rejected rather
 * than silently resolved.
 */
function datasourceSchemas(sources: readonly SQLSource[]): Map<string, string> {
  try {
    return resolveDatasourceSchemas(
      sources.map((src) => ({ datasource: src.datasource, schema: src.schema, namespace: src.namespace })),
    );
  } catch (error) {
    if (error instanceof DatasourceSchemaConflictError) {
      throw new MigrationError(error.message, 'sql', error);
    }
    throw error;
  }
}
