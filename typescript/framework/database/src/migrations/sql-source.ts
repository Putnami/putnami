import {
  type BundleOperation,
  type BundlePayload,
  canonicalMigrationId,
  DEFAULT_DATASOURCE,
  type InfraDatabaseRequirement,
  type Kind,
  KindSQL,
  type MigrationBundleContributor,
  type MigrationSource,
} from '@putnami/migration';
import { sqlBundleContributions } from './bundle';

/**
 * Fully describes the database a SQL migration source targets, separating
 * two concerns:
 *
 * - `name` is the logical database the source migrates against. It matches the
 *   pool's database and scopes a migration's canonical id and bundle target.
 * - `schema` is the schema the source's SQL actually creates its objects in,
 *   declared explicitly rather than inferred from the migration `namespace`
 *   (which is the source's identity, almost never the schema name) — so the
 *   emitted infra/requirements.json names schemas that exist. The runner also
 *   applies `schema` as the transaction-local `search_path` before each
 *   migration body, so a source's unqualified DDL lands in it and one migration
 *   set can target many schemas via distinct datasource pairs. The schema never
 *   participates in a migration's canonical id or the bundle digest — it is a
 *   deploy-time/runtime concern, not a payload one.
 *
 * Mirrors Go's `database.Datasource`, so the two frameworks stay symmetric.
 */
export interface Datasource {
  name: string;
  schema: string;
}

/**
 * A single SQL migration definition. Authors typically don't write
 * these directly — the recommended path is `sqlSourceInline({
 * namespace, datasource, definitions })` with `definitions` coming
 * from an imported `.migrations.gen.ts` (text imports baked in at
 * build time by the SQL plugin's generator).
 *
 * The `up`, `down`, `namespace`, `datasource`, and `source` fields
 * match the Go runner's `database.Definition` field-for-field so the
 * cross-language state-store remains consistent.
 */
export interface SQLDefinition {
  /**
   * Framework-visible identifier. The canonical form is
   * "<namespace>/<basename>" (e.g. "iam/20260520120000_create_users");
   * the SQLSource constructor stamps the prefix when missing.
   */
  name: string;
  /**
   * The up migration content. Executed atomically inside a single
   * transaction together with the state-store insert, so a partial failure
   * (crash/timeout) leaves neither the schema change nor its bookkeeping row.
   * Consequently, statements that cannot run inside a transaction (e.g.
   * `CREATE INDEX CONCURRENTLY`) must not be placed in an up migration.
   */
  sql: string;
  /** The optional rollback SQL. Empty means non-reversible. */
  down?: string;
  /** Namespace stamped by the source at registration time. */
  namespace?: string;
  /** Datasource stamped by the source at registration time. */
  datasource?: string;
  /** Diagnostic origin: e.g. "embed:iam/migrations/..." or "inline:iam". */
  source?: string;
}

/**
 * The SQL flavor of `MigrationSource`. Carries a slice of inline
 * `SQLDefinition`s (typically imported from a generated module that
 * inlines `.up.sql` / `.down.sql` text via `with { type: 'text' }`).
 *
 * Multiple `SQLSource`s for the same datasource merge cleanly;
 * duplicate-name detection across sources happens at apply time.
 */
export class SQLSource implements MigrationSource, MigrationBundleContributor {
  readonly kind: Kind = KindSQL;
  readonly namespace: string;
  readonly datasource: string;
  readonly schema: string;
  readonly definitions: readonly SQLDefinition[];

  constructor(opts: { namespace: string; datasource: Datasource; definitions: readonly SQLDefinition[] }) {
    if (!opts.namespace || opts.namespace.trim() === '') {
      throw new Error('SQLSource requires a non-empty namespace');
    }
    this.namespace = opts.namespace;
    this.datasource = opts.datasource.name || DEFAULT_DATASOURCE;
    this.schema = (opts.datasource.schema ?? '').trim();
    this.definitions = opts.definitions.map((d) => this.stamp(d));
  }

  /**
   * Infra contribution: an SQL source migrates the schema it declares on its
   * `datasource` (a Postgres database today). The schema is declared, not
   * inferred from the `namespace` — that decoupling is what makes the emitted
   * infra/requirements.json name schemas that actually exist. The migration
   * framework merges these across sources into the per-project infra scratch
   * fragment. A source that declares no schema contributes only the database.
   */
  infraDatabase(): InfraDatabaseRequirement {
    const requirement: InfraDatabaseRequirement = { name: this.datasource, engine: 'postgres' };
    if (this.schema) requirement.schemas = [this.schema];
    return requirement;
  }

  /**
   * Bundle contribution: expand this source into bundle operations and
   * payloads. Implementing it per-source (not only via the SQL runner) is what
   * lets the build emit a faithful bundle straight from the contributed
   * sources, independent of the runtime persistence backend. Mirrors Go's
   * source-level `protocolmigration.BundleContributor`.
   */
  migrationBundleOperations(): { operations: BundleOperation[]; payloads: BundlePayload[] } {
    return sqlBundleContributions([this]);
  }

  private stamp(d: SQLDefinition): SQLDefinition {
    const ns = this.namespace;
    const out: SQLDefinition = { ...d };
    if (!out.namespace) out.namespace = ns;
    if (!out.datasource) out.datasource = this.datasource;
    if (!out.source) out.source = `inline:${ns}`;
    if (out.name && !out.name.includes('/')) {
      out.name = `${ns}/${out.name}`;
    }
    return out;
  }
}

/**
 * Convenience factory mirroring Go's `database.NewSQLSource`. Authors
 * typically write:
 *
 * ```ts
 * import { iamMigrations } from './.gen/migrations.gen';
 *
 * migrationSources(): MigrationSource[] {
 *   return [sqlSourceInline({
 *     namespace: 'iam',
 *     datasource: { name: 'default', schema: 'identity' },
 *     definitions: iamMigrations,
 *   })];
 * }
 * ```
 *
 * The generator produces `iamMigrations` by walking each contributing
 * package's `migrations/` directory and emitting text imports for
 * every `.up.sql` / `.down.sql` pair.
 */
export function sqlSourceInline(opts: {
  namespace: string;
  datasource: Datasource;
  definitions: readonly SQLDefinition[];
}): SQLSource {
  return new SQLSource(opts);
}

/** Reserved for the codegen-driven path. The generator emits per-package
 *  `__pkg_migrations` constants; this factory wraps one into a Source.
 *  Equivalent to sqlSourceInline; documented separately so the call
 *  site reads "from generated file" vs "inline".
 */
export function sqlSourceFromGenerated(opts: {
  namespace: string;
  datasource: Datasource;
  definitions: readonly SQLDefinition[];
}): SQLSource {
  return new SQLSource(opts);
}

/** Canonical state-store id for an SQL migration row. */
export function sqlMigrationId(source: SQLSource, name: string): string {
  return canonicalMigrationId(source.datasource, name);
}
