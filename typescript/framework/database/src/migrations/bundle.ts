import {
  type BundleOperation,
  type BundlePayload,
  computePayloadHash,
  DEFAULT_DATASOURCE,
  KindSQL,
} from '@putnami/migration';
import type { SQLSource } from './sql-source';

/**
 * Maps SQL sources to migration-bundle operations and their materialized
 * `.up.sql`/`.down.sql` payloads. Pure and synchronous — no database
 * connection — so emission stays a reproducible build step. Payload hashes use
 * the same SHA-256 the runner stamps at apply time, so a bundled migration and
 * an applied one share one content hash.
 *
 * Counterpart of Go's `database.(*SQLRunner).MigrationBundleOperations`.
 */
export function sqlBundleContributions(sources: readonly SQLSource[]): {
  operations: BundleOperation[];
  payloads: BundlePayload[];
} {
  const operations: BundleOperation[] = [];
  const payloads: BundlePayload[] = [];

  for (const source of sources) {
    for (const def of source.definitions) {
      const target = def.datasource || DEFAULT_DATASOURCE;
      const upPath = bundleSqlPayloadPath(target, def.name, '.up.sql');

      const op: BundleOperation = {
        kind: KindSQL,
        target,
        // Carried so an applier reconstructs a schema-aware source. Emitted
        // only when non-empty to mirror Go's `omitempty`, keeping schema-less
        // bundles byte- and digest-identical across both runtimes.
        ...(source.schema ? { schema: source.schema } : {}),
        namespace: def.namespace,
        name: def.name,
        orderKey: def.name,
        up: { path: upPath, hash: computePayloadHash(def.sql) },
        safety: 'safe-online',
        capabilities: { transactional: true },
      };
      payloads.push({ path: upPath, bytes: def.sql });

      if (def.down) {
        const downPath = bundleSqlPayloadPath(target, def.name, '.down.sql');
        op.down = { path: downPath, hash: computePayloadHash(def.down) };
        op.capabilities = { transactional: true, reversible: true };
        payloads.push({ path: downPath, bytes: def.down });
      }

      operations.push(op);
    }
  }

  return { operations, payloads };
}

/**
 * Canonical in-bundle path for a SQL payload:
 * `payload/sql/<target>/<namespace>/<basename>.{up,down}.sql`. `name` already
 * carries the `<namespace>/<basename>` form stamped by the source.
 */
function bundleSqlPayloadPath(target: string, name: string, suffix: string): string {
  return `payload/sql/${target}/${name}${suffix}`;
}
