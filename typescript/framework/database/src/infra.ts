import { DEFAULT_DATASOURCE } from '@putnami/migration';
import { isTableDefinition, type TableDefinition } from './table';

/**
 * URL of the per-project infra-requirements JSON Schema. Written into the
 * emitted manifest so editors validate it against the published contract.
 */
const INFRA_SCHEMA_URL = 'https://putnami.dev/schemas/putnami-infra.json';

/** Current infra-requirements protocol version (see protocols/infra). */
const PROTOCOL_VERSION = 2 as const;

/**
 * Database engine emitted for `@putnami/database` requirements. The
 * framework is PostgreSQL-only, so every pool maps to "postgres".
 */
export const DATABASE_ENGINE = 'postgres';

/**
 * The closed v1 set of engines the infra protocol accepts. Mirrors
 * `go.putnami.dev/protocol/infra.ValidEngines` — TS does not import the Go
 * package, so this must stay in sync with the enum in
 * `protocols/infra/schemas/infra.json`. Adding an engine requires a
 * protocol-version bump on both sides.
 */
export const VALID_ENGINES = ['postgres', 'mysql', 'sqlite'] as const;

export type InfraEngine = (typeof VALID_ENGINES)[number];

/** A single database requirement, conforming to the `database` shape of the schema. */
export interface DatabaseRequirement {
  name: string;
  engine: InfraEngine;
  schemas?: string[];
}

/** A per-project infra-requirements manifest carrying only `databases`. */
export interface InfraRequirements {
  $schema: string;
  protocolVersion: typeof PROTOCOL_VERSION;
  databases?: DatabaseRequirement[];
}

/**
 * Validate an engine against the closed v1 enum, returning it narrowed to
 * `InfraEngine`. Throws a clear diagnostic for anything outside the set so a
 * misconfiguration surfaces at generate time rather than reaching a deployer.
 */
export function toInfraEngine(engine: string): InfraEngine {
  if ((VALID_ENGINES as readonly string[]).includes(engine)) {
    return engine as InfraEngine;
  }
  throw new Error(
    `@putnami/database: unsupported database engine "${engine}"; infra protocol v1 accepts ${VALID_ENGINES.join(', ')}`,
  );
}

/**
 * Collect the exported `TableDefinition` values from a loaded module. Used by
 * the SQL plugin's `generate()` hook to materialise table definitions from the
 * project's local table files and dependency packages.
 */
export function collectTableDefinitions(module: Record<string, unknown>): TableDefinition[] {
  const tables: TableDefinition[] = [];
  for (const value of Object.values(module)) {
    if (isTableDefinition(value)) {
      tables.push(value);
    }
  }
  return tables;
}

/**
 * Group table definitions by datasource (pool) into infra database
 * requirements: one entry per pool, with `schemas` the sorted unique set of
 * non-empty schema names its tables declare. Tables without an explicit `db`
 * fall back to `defaultDatasource` — the workload's declared primary datasource
 * (`sql({ datasource })`) when set, otherwise the framework default — so the
 * emitted requirement names the same datasource the runtime reads/writes.
 * Entries are sorted by name for deterministic output.
 */
export function databasesFromTables(
  tables: readonly TableDefinition[],
  defaultDatasource: string = DEFAULT_DATASOURCE,
): DatabaseRequirement[] {
  const fallback = defaultDatasource || DEFAULT_DATASOURCE;
  const pools = new Map<string, Set<string>>();
  for (const table of tables) {
    const pool = table.options.db || fallback;
    const schemas = pools.get(pool) ?? new Set<string>();
    if (table.options.schema) {
      schemas.add(table.options.schema);
    }
    pools.set(pool, schemas);
  }

  const engine = toInfraEngine(DATABASE_ENGINE);
  return [...pools.entries()]
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([name, schemas]) => {
      const entry: DatabaseRequirement = { name, engine };
      if (schemas.size > 0) {
        entry.schemas = [...schemas].sort((a, b) => a.localeCompare(b));
      }
      return entry;
    });
}

/**
 * Build the per-project infra-requirements manifest from collected table
 * definitions. Tables without an explicit `db` fall back to `defaultDatasource`
 * (the plugin's declared primary datasource, when set). Returns `undefined` when
 * no pools are declared so the caller can skip writing an empty sidecar.
 */
export function buildInfraRequirements(
  tables: readonly TableDefinition[],
  defaultDatasource?: string,
): InfraRequirements | undefined {
  const databases = databasesFromTables(tables, defaultDatasource);
  if (databases.length === 0) {
    return undefined;
  }
  return {
    $schema: INFRA_SCHEMA_URL,
    protocolVersion: PROTOCOL_VERSION,
    databases,
  };
}
