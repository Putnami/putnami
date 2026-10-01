import type { CollectionDefinition } from './collection';

/** infra-requirements protocol version this framework emits. */
export const INFRA_PROTOCOL_VERSION = 2;

/** Published JSON Schema id for a per-project infra-requirements manifest. */
export const INFRA_SCHEMA_URL = 'https://putnami.dev/schemas/putnami-infra.json';

/** Default named store used by collections that do not set `options.db`. */
export const DEFAULT_STORE = 'default';

export interface InfraDatabase {
  readonly name: string;
  readonly engine: 'firestore';
  readonly schemas: string[];
}

export interface InfraManifest {
  readonly $schema: string;
  readonly protocolVersion: number;
  readonly databases: InfraDatabase[];
}

/**
 * Map a project's registered document collections to a per-project infra
 * requirements manifest.
 *
 * Collections are grouped by their named store (`options.db`, defaulting to
 * `"default"`). Each store resolves its backend independently — runtime reads
 * a named store's config at `document.<store>` (see factory.ts) — so the
 * caller supplies `resolveBackend(store)`. Only stores whose backend resolves
 * to `firestore` become a database entry; the in-memory backend (tests, local
 * dev) is not deployed infrastructure and emits nothing.
 *
 * Returns `null` when no store maps to a firestore database (no collections,
 * or every store is in-memory). Output is deterministic: databases are sorted
 * by store name and schemas within each database are sorted and de-duplicated.
 */
export function buildDocumentInfraManifest(
  collections: readonly CollectionDefinition[],
  resolveBackend: (store: string) => string,
): InfraManifest | null {
  const byStore = new Map<string, Set<string>>();
  for (const collection of collections) {
    const store = collection.options.db ?? DEFAULT_STORE;
    const names = byStore.get(store) ?? new Set<string>();
    names.add(collection.collectionName);
    byStore.set(store, names);
  }

  const databases: InfraDatabase[] = Array.from(byStore.entries())
    .sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))
    .filter(([store]) => resolveBackend(store) === 'firestore')
    .map(([name, schemas]) => ({
      name,
      engine: 'firestore',
      schemas: Array.from(schemas).sort(),
    }));

  if (databases.length === 0) {
    return null;
  }

  return {
    $schema: INFRA_SCHEMA_URL,
    protocolVersion: INFRA_PROTOCOL_VERSION,
    databases,
  };
}
