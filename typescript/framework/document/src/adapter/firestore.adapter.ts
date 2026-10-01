import { DocumentError, DocumentErrorCode } from '../errors';
import type { Consistency } from '../repository/query';
import type {
  AdapterCapabilities,
  AdapterFilter,
  AdapterFindOptions,
  AdapterFindResult,
  AdapterId,
  AdapterTx,
  DocumentAdapter,
} from './document.adapter';

const FIRESTORE_BATCH_WRITE_LIMIT = 500;
const FIRESTORE_POST_FILTER_SCAN_MIN = 50;
const FIRESTORE_POST_FILTER_SCAN_MAX = 500;
const FIRESTORE_POST_FILTER_SCAN_MULTIPLIER = 2;

type FirestoreAdapterConfig = {
  projectId?: string;
  databaseId?: string;
  emulatorHost?: string;
  credentials?: string;
};

function encodeCursor(lastId: string | undefined): string | undefined {
  if (!lastId) return undefined;
  return Buffer.from(JSON.stringify({ lastId }), 'utf8').toString('base64url');
}

function decodeCursor(cursor?: string): { lastId: string } | undefined {
  if (!cursor) return undefined;
  try {
    const decoded = JSON.parse(Buffer.from(cursor, 'base64url').toString('utf8')) as { lastId?: string };
    if (!decoded?.lastId) {
      throw new Error('Missing lastId');
    }
    return { lastId: decoded.lastId };
  } catch (error) {
    throw new DocumentError(
      `Invalid cursor: ${error instanceof Error ? error.message : String(error)}`,
      DocumentErrorCode.InvalidCursor,
      error instanceof Error ? error : undefined,
    );
  }
}

function isPostFilter(filter: AdapterFilter): boolean {
  return filter.op === 'exists' && filter.value === false;
}

function isNegativeFilter(filter: AdapterFilter): boolean {
  return filter.op === 'ne' || filter.op === 'notIn' || (filter.op === 'exists' && filter.value === true);
}

function matchesPostFilter(document: Record<string, unknown>, filter: AdapterFilter): boolean {
  if (filter.op !== 'exists' || filter.value !== false) {
    return true;
  }
  const value = document[filter.field];
  return value === undefined || value === null;
}

function toFirestoreId(id: AdapterId): string {
  if (typeof id === 'string' || typeof id === 'number') {
    return String(id);
  }
  throw new DocumentError(
    'Firestore adapter does not support composite document ids',
    DocumentErrorCode.CompositeIdNotSupported,
  );
}

/**
 * Internal: normalize the `document.credentials` setting into the shape the
 * Firestore client expects. A value starting with `{` is parsed as inline JSON;
 * anything else is treated as a key file path. Exported for unit testing only —
 * it is not re-exported from the package entry point.
 */
export function parseCredentials(credentials?: string): {
  credentials?: Record<string, unknown>;
  keyFilename?: string;
} {
  if (!credentials) return {};
  const trimmed = credentials.trim();
  if (trimmed.startsWith('{')) {
    try {
      return { credentials: JSON.parse(trimmed) as Record<string, unknown> };
    } catch (error) {
      // Never include `credentials` in the message — it is a secret.
      throw new DocumentError(
        'The "document.credentials" setting starts with "{" but is not valid JSON. Provide a valid service-account JSON blob or a key file path.',
        DocumentErrorCode.InvalidCredentials,
        error instanceof Error ? error : undefined,
      );
    }
  }
  return { keyFilename: credentials };
}

/**
 * Minimal structural view of the `@google-cloud/firestore` surface this adapter
 * actually uses. We deliberately avoid importing the firebase types so the SDK
 * stays an optional peer dependency; the real client is a structural superset of
 * these interfaces, so it assigns without casts.
 */

/** Firestore's `where` operator strings. */
type FirestoreWhereOp = '==' | '!=' | '>' | '>=' | '<' | '<=' | 'in' | 'not-in' | 'array-contains';

interface FirestoreDocumentSnapshot {
  readonly id: string;
  readonly exists: boolean;
  readonly ref: FirestoreDocumentRef;
  data(): Record<string, unknown> | undefined;
}

interface FirestoreQuerySnapshot {
  readonly docs: FirestoreDocumentSnapshot[];
}

interface FirestoreQuery {
  where(field: string, op: FirestoreWhereOp, value: unknown): FirestoreQuery;
  orderBy(field: string, direction: 'asc' | 'desc'): FirestoreQuery;
  startAfter(snapshot: FirestoreDocumentSnapshot): FirestoreQuery;
  limit(limit: number): FirestoreQuery;
  get(): Promise<FirestoreQuerySnapshot>;
}

interface FirestoreDocumentRef {
  get(): Promise<FirestoreDocumentSnapshot>;
  set(data: Record<string, unknown>): Promise<unknown>;
  delete(): Promise<unknown>;
}

interface FirestoreCollectionRef extends FirestoreQuery {
  doc(id: string): FirestoreDocumentRef;
}

interface FirestoreWriteBatch {
  set(ref: FirestoreDocumentRef, data: Record<string, unknown>): unknown;
  delete(ref: FirestoreDocumentRef): unknown;
  commit(): Promise<unknown>;
}

interface FirestoreTransaction {
  get(target: FirestoreDocumentRef): Promise<FirestoreDocumentSnapshot>;
  get(target: FirestoreQuery): Promise<FirestoreQuerySnapshot>;
  set(ref: FirestoreDocumentRef, data: Record<string, unknown>): unknown;
  delete(ref: FirestoreDocumentRef): unknown;
}

interface FirestoreClient {
  collection(name: string): FirestoreCollectionRef;
  batch(): FirestoreWriteBatch;
  runTransaction<R>(fn: (transaction: FirestoreTransaction) => Promise<R>): Promise<R>;
  // Optional: batched multi-document read in a single round-trip, preserving the
  // order of the supplied refs. The real `@google-cloud/firestore` client always
  // provides it; typed optional so a minimal/fake client can omit it and callers
  // fall back to parallel per-ref `get()`s.
  getAll?(...refs: FirestoreDocumentRef[]): Promise<FirestoreDocumentSnapshot[]>;
  close?(): Promise<void> | void;
}

type FirestoreModule = {
  // `any` is confined to this dynamic-import boundary: the constructor options
  // and instance shape vary across SDK versions. The instance is immediately
  // re-typed through the narrow FirestoreClient interface above, keeping the
  // SDK's version-dependent surface out of the rest of this adapter.
  Firestore: new (
    options?: Record<string, unknown>,
  ) => any;
};

async function importFirestore(): Promise<FirestoreModule> {
  try {
    const moduleName = '@google-cloud/firestore';
    return (await import(moduleName)) as FirestoreModule;
  } catch (error) {
    throw new DocumentError(
      'Firestore support requires the optional peer dependency "@google-cloud/firestore". Install it with `putnami deps add @google-cloud/firestore --project /typescript/framework/document`.',
      DocumentErrorCode.AdapterNotInstalled,
      error instanceof Error ? error : undefined,
    );
  }
}

export class FirestoreAdapter implements DocumentAdapter {
  private clientPromise?: Promise<FirestoreClient>;

  readonly capabilities: AdapterCapabilities = {
    strongConsistency: true,
    eventualConsistency: true,
    transactions: true,
    contains: true,
    exists: true,
    compositeIds: false,
    strictIndexes: false,
  };

  constructor(
    public readonly storeName = 'default',
    private readonly config: FirestoreAdapterConfig = {},
  ) {}

  private async client(): Promise<FirestoreClient> {
    this.clientPromise ??= (async () => {
      const { Firestore } = await importFirestore();
      if (this.config.emulatorHost) {
        process.env['FIRESTORE_EMULATOR_HOST'] ??= this.config.emulatorHost;
      }

      return new Firestore({
        projectId: this.config.projectId,
        databaseId: this.config.databaseId,
        ...parseCredentials(this.config.credentials),
      }) as FirestoreClient;
    })();

    return this.clientPromise;
  }

  private assertFilterCompatibility(filters: AdapterFilter[]): void {
    const negativeFilters = filters.filter(isNegativeFilter);
    if (negativeFilters.length <= 1) return;

    const fields = Array.from(new Set(negativeFilters.map((filter) => filter.field)));
    if (fields.length > 1) {
      throw new DocumentError(
        `Firestore does not support negative filters on multiple fields in the same query: ${fields.join(', ')}`,
        DocumentErrorCode.QueryNotSupported,
      );
    }

    throw new DocumentError(
      `Firestore does not support multiple negative filters in the same query on field "${fields[0]}"`,
      DocumentErrorCode.QueryNotSupported,
    );
  }

  private async resolveCursorSnapshot(
    collection: string,
    cursor: string | undefined,
    tx?: FirestoreTransaction,
  ): Promise<FirestoreDocumentSnapshot | undefined> {
    if (!cursor) return undefined;

    const client = await this.client();
    const decodedCursor = decodeCursor(cursor);
    if (!decodedCursor) {
      throw new DocumentError('Cursor is invalid', DocumentErrorCode.InvalidCursor);
    }

    const ref = client.collection(collection).doc(decodedCursor.lastId);
    const snapshot = tx ? await tx.get(ref) : await ref.get();
    if (!snapshot?.exists) {
      throw new DocumentError('Cursor no longer points to an existing document', DocumentErrorCode.InvalidCursor);
    }

    return snapshot;
  }

  private postFilterQueryLimit(limit: number): number {
    return Math.min(
      FIRESTORE_POST_FILTER_SCAN_MAX,
      Math.max(limit + 1, limit * FIRESTORE_POST_FILTER_SCAN_MULTIPLIER, FIRESTORE_POST_FILTER_SCAN_MIN),
    );
  }

  private async baseQuery(
    collection: string,
    filters: AdapterFilter[],
    options: AdapterFindOptions,
    tx?: FirestoreTransaction,
    queryLimit?: number,
    startAfterSnapshot?: FirestoreDocumentSnapshot,
  ): Promise<FirestoreQuery> {
    const client = await this.client();
    let query: FirestoreQuery = client.collection(collection);
    this.assertFilterCompatibility(filters);

    for (const filter of filters) {
      if (isPostFilter(filter)) continue;

      switch (filter.op) {
        case 'eq':
          query = query.where(filter.field, '==', filter.value);
          break;
        case 'ne':
          query = query.where(filter.field, '!=', filter.value);
          break;
        case 'gt':
          query = query.where(filter.field, '>', filter.value);
          break;
        case 'gte':
          query = query.where(filter.field, '>=', filter.value);
          break;
        case 'lt':
          query = query.where(filter.field, '<', filter.value);
          break;
        case 'lte':
          query = query.where(filter.field, '<=', filter.value);
          break;
        case 'in':
          query = query.where(filter.field, 'in', filter.value);
          break;
        case 'notIn':
          query = query.where(filter.field, 'not-in', filter.value);
          break;
        case 'contains':
          query = query.where(filter.field, 'array-contains', filter.value);
          break;
        case 'exists':
          if (filter.value === true) {
            query = query.where(filter.field, '!=', null);
          }
          break;
      }
    }

    for (const order of options.orderBy) {
      query = query.orderBy(order.field, order.direction);
    }

    const cursorSnapshot = startAfterSnapshot ?? (await this.resolveCursorSnapshot(collection, options.cursor, tx));
    if (cursorSnapshot) {
      query = query.startAfter(cursorSnapshot);
    }

    if (queryLimit !== undefined) {
      query = query.limit(queryLimit);
    }

    return query;
  }

  private async fetchQueryDocs(
    collection: string,
    filters: AdapterFilter[],
    options: AdapterFindOptions,
    tx?: FirestoreTransaction,
    queryLimit?: number,
    startAfterSnapshot?: FirestoreDocumentSnapshot,
  ): Promise<FirestoreDocumentSnapshot[]> {
    const query = await this.baseQuery(collection, filters, options, tx, queryLimit, startAfterSnapshot);
    const snapshot = tx ? await tx.get(query) : await query.get();
    return snapshot.docs;
  }

  private async runQuery(
    collection: string,
    filters: AdapterFilter[],
    options: AdapterFindOptions,
    tx?: FirestoreTransaction,
  ): Promise<AdapterFindResult> {
    const queryLimit = filters.some(isPostFilter) ? this.postFilterQueryLimit(options.limit) : options.limit + 1;
    const docs: FirestoreDocumentSnapshot[] = [];
    let startAfterSnapshot = await this.resolveCursorSnapshot(collection, options.cursor, tx);

    while (docs.length <= options.limit) {
      const batch = await this.fetchQueryDocs(collection, filters, options, tx, queryLimit, startAfterSnapshot);
      if (batch.length === 0) break;

      for (const doc of batch) {
        if (filters.every((filter) => matchesPostFilter(doc.data() as Record<string, unknown>, filter))) {
          docs.push(doc);
          if (docs.length > options.limit) {
            break;
          }
        }
      }

      if (docs.length > options.limit || batch.length < queryLimit) {
        break;
      }

      startAfterSnapshot = batch.at(-1);
    }

    const page = docs.slice(0, options.limit);
    const nextCursor = docs.length > options.limit ? encodeCursor(page.at(-1)?.id) : undefined;

    return {
      items: page.map((doc) => (doc.data() ?? {}) as Record<string, unknown>),
      nextCursor,
    };
  }

  async get(
    collection: string,
    id: AdapterId,
    _consistency: Consistency,
  ): Promise<Record<string, unknown> | undefined> {
    const snapshot = await (await this.client()).collection(collection).doc(toFirestoreId(id)).get();
    return snapshot.exists ? ((snapshot.data() ?? {}) as Record<string, unknown>) : undefined;
  }

  async exists(collection: string, id: AdapterId, consistency: Consistency): Promise<boolean> {
    return (await this.get(collection, id, consistency)) !== undefined;
  }

  async find(collection: string, filters: AdapterFilter[], options: AdapterFindOptions): Promise<AdapterFindResult> {
    return this.runQuery(collection, filters, options);
  }

  async save(collection: string, id: AdapterId, doc: Record<string, unknown>): Promise<Record<string, unknown>> {
    // Adapter save contract: return the *stored* document by re-reading after
    // the write, so any server-applied write transform (e.g. Firestore field
    // sentinels) is reflected consistently with the in-memory adapter. Echoing
    // the request body would silently diverge from that contract.
    const ref = (await this.client()).collection(collection).doc(toFirestoreId(id));
    await ref.set(doc);
    const snapshot = await ref.get();
    return (snapshot.data() ?? { ...doc }) as Record<string, unknown>;
  }

  async saveMany(
    collection: string,
    items: Array<{ id: AdapterId; data: Record<string, unknown> }>,
  ): Promise<Record<string, unknown>[]> {
    const client = await this.client();
    const saved: Record<string, unknown>[] = [];

    for (let index = 0; index < items.length; index += FIRESTORE_BATCH_WRITE_LIMIT) {
      const chunk = items.slice(index, index + FIRESTORE_BATCH_WRITE_LIMIT);
      const batch = client.batch();
      const writes = chunk.map((item) => ({
        ref: client.collection(collection).doc(toFirestoreId(item.id)),
        data: item.data,
      }));
      for (const write of writes) {
        batch.set(write.ref, write.data);
      }
      await batch.commit();
      // Re-read each written document to honor the adapter save contract: the
      // return value reflects the *stored* document, not the request body. Batch
      // the re-reads into a single `getAll` round-trip (order-preserving) instead
      // of N sequential `get()`s. Fall back to parallel per-ref reads when the
      // client does not expose `getAll`.
      const refs = writes.map((write) => write.ref);
      const snapshots = client.getAll ? await client.getAll(...refs) : await Promise.all(refs.map((ref) => ref.get()));
      for (let i = 0; i < writes.length; i += 1) {
        saved.push((snapshots[i]?.data() ?? { ...writes[i].data }) as Record<string, unknown>);
      }
    }

    return saved;
  }

  async delete(collection: string, id: AdapterId): Promise<Record<string, unknown> | undefined> {
    const client = await this.client();
    const ref = client.collection(collection).doc(toFirestoreId(id));
    const existing = await ref.get();
    if (!existing.exists) return undefined;
    const data = (existing.data() ?? {}) as Record<string, unknown>;
    await ref.delete();
    return data;
  }

  async deleteMany(collection: string, filters: AdapterFilter[]): Promise<number> {
    const client = await this.client();
    let deleted = 0;
    let startAfterSnapshot: FirestoreDocumentSnapshot | undefined;

    while (true) {
      const docs = await this.fetchQueryDocs(
        collection,
        filters,
        {
          limit: FIRESTORE_BATCH_WRITE_LIMIT,
          orderBy: [],
          consistency: 'strong',
        },
        undefined,
        FIRESTORE_BATCH_WRITE_LIMIT,
        startAfterSnapshot,
      );

      if (docs.length === 0) {
        break;
      }

      const chunk = docs.filter((doc) =>
        filters.every((filter) => matchesPostFilter((doc.data() ?? {}) as Record<string, unknown>, filter)),
      );

      if (chunk.length > 0) {
        const batch = client.batch();
        for (const doc of chunk) {
          batch.delete(doc.ref);
          deleted++;
        }
        await batch.commit();
      }

      if (docs.length < FIRESTORE_BATCH_WRITE_LIMIT) {
        break;
      }

      startAfterSnapshot = docs.at(-1);
    }

    return deleted;
  }

  async runInTransaction<R>(fn: (tx: AdapterTx) => Promise<R>): Promise<R> {
    const client = await this.client();
    return client.runTransaction(async (transaction: FirestoreTransaction) =>
      fn({
        get: async (collection, id) => {
          const snapshot = await transaction.get(client.collection(collection).doc(toFirestoreId(id)));
          return snapshot.exists ? ((snapshot.data() ?? {}) as Record<string, unknown>) : undefined;
        },
        find: async (collection, filters, options) => this.runQuery(collection, filters, options, transaction),
        save: async (collection, id, doc) => {
          transaction.set(client.collection(collection).doc(toFirestoreId(id)), doc);
        },
        delete: async (collection, id) => {
          transaction.delete(client.collection(collection).doc(toFirestoreId(id)));
        },
      }),
    );
  }

  async close(): Promise<void> {
    if (!this.clientPromise) return;
    const client = await this.clientPromise;
    await client.close?.();
    this.clientPromise = undefined;
  }
}
