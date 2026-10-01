import { DocumentError, DocumentErrorCode } from '../errors';
import type {
  AdapterCapabilities,
  AdapterFilter,
  AdapterFindOptions,
  AdapterFindResult,
  AdapterId,
  AdapterTx,
  DocumentAdapter,
} from './document.adapter';

function cloneValue<T>(value: T): T {
  return structuredClone(value);
}

function serializeId(id: AdapterId): string {
  if (typeof id === 'string') return `s:${id}`;
  if (typeof id === 'number') return `n:${id}`;

  const entries = Object.entries(id).sort(([left], [right]) => left.localeCompare(right));
  return `o:${JSON.stringify(entries)}`;
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

function encodeCursor(lastId: string | undefined): string | undefined {
  if (!lastId) return undefined;
  return Buffer.from(JSON.stringify({ lastId }), 'utf8').toString('base64url');
}

function compareValues(left: unknown, right: unknown): number {
  if (left === right) return 0;
  if (left === undefined || left === null) return 1;
  if (right === undefined || right === null) return -1;

  if (typeof left === 'number' && typeof right === 'number') {
    return left - right;
  }
  if (typeof left === 'boolean' && typeof right === 'boolean') {
    return Number(left) - Number(right);
  }

  return String(left).localeCompare(String(right));
}

function matchesFilter(document: Record<string, unknown>, filter: AdapterFilter): boolean {
  const value = document[filter.field];
  const exists = value !== undefined && value !== null;

  switch (filter.op) {
    case 'eq':
      return exists || filter.value === null ? value === filter.value : false;
    case 'ne':
      return exists && value !== filter.value;
    case 'gt':
      return exists && compareValues(value, filter.value) > 0;
    case 'gte':
      return exists && compareValues(value, filter.value) >= 0;
    case 'lt':
      return exists && compareValues(value, filter.value) < 0;
    case 'lte':
      return exists && compareValues(value, filter.value) <= 0;
    case 'in':
      return exists && Array.isArray(filter.value) && filter.value.some((item) => item === value);
    case 'notIn':
      return exists && value !== null && Array.isArray(filter.value) && !filter.value.some((item) => item === value);
    case 'contains':
      return Array.isArray(value) && value.includes(filter.value);
    case 'exists':
      return filter.value === true ? exists : !exists;
    default:
      return false;
  }
}

type Store = Map<string, Map<string, Record<string, unknown>>>;

function cloneStore(source: Store): Store {
  const cloned: Store = new Map();
  for (const [collection, docs] of source.entries()) {
    const clonedDocs = new Map<string, Record<string, unknown>>();
    for (const [id, document] of docs.entries()) {
      clonedDocs.set(id, cloneValue(document));
    }
    cloned.set(collection, clonedDocs);
  }
  return cloned;
}

function findInStore(
  store: Store,
  collection: string,
  filters: AdapterFilter[],
  options: AdapterFindOptions,
): AdapterFindResult {
  const docs = Array.from(store.get(collection)?.entries() ?? []).map(([serializedId, document]) => ({
    serializedId,
    document,
  }));

  const matching = docs.filter(({ document }) => filters.every((filter) => matchesFilter(document, filter)));
  matching.sort((left, right) => {
    for (const order of options.orderBy) {
      const compared = compareValues(left.document[order.field], right.document[order.field]);
      if (compared !== 0) {
        return order.direction === 'desc' ? compared * -1 : compared;
      }
    }
    return left.serializedId.localeCompare(right.serializedId);
  });

  const cursor = decodeCursor(options.cursor);
  const startIndex = cursor ? matching.findIndex((item) => item.serializedId === cursor.lastId) + 1 : 0;
  if (cursor && startIndex === 0) {
    throw new DocumentError('Cursor no longer matches a document in this result set', DocumentErrorCode.InvalidCursor);
  }

  const page = matching.slice(startIndex, startIndex + options.limit);
  const nextCursor = matching.length > startIndex + options.limit ? encodeCursor(page.at(-1)?.serializedId) : undefined;

  return {
    items: page.map(({ document }) => cloneValue(document)),
    nextCursor,
  };
}

function txForStore(store: Store): AdapterTx {
  return {
    async get(collection, id) {
      return cloneValue(store.get(collection)?.get(serializeId(id)));
    },
    async find(collection, filters, options) {
      return findInStore(store, collection, filters, options);
    },
    async save(collection, id, doc) {
      const docs = store.get(collection) ?? new Map<string, Record<string, unknown>>();
      docs.set(serializeId(id), cloneValue(doc));
      store.set(collection, docs);
    },
    async delete(collection, id) {
      store.get(collection)?.delete(serializeId(id));
    },
  };
}

export class MemoryAdapter implements DocumentAdapter {
  private store: Store = new Map();
  private mutex: Promise<void> = Promise.resolve();

  readonly capabilities: AdapterCapabilities = {
    strongConsistency: true,
    eventualConsistency: true,
    transactions: true,
    contains: true,
    exists: true,
    compositeIds: true,
    strictIndexes: false,
  };

  constructor(public readonly storeName = 'default') {}

  private collection(collection: string): Map<string, Record<string, unknown>> {
    const docs = this.store.get(collection) ?? new Map<string, Record<string, unknown>>();
    this.store.set(collection, docs);
    return docs;
  }

  clear(): void {
    this.store.clear();
  }

  async get(collection: string, id: AdapterId): Promise<Record<string, unknown> | undefined> {
    return cloneValue(this.collection(collection).get(serializeId(id)));
  }

  async exists(collection: string, id: AdapterId): Promise<boolean> {
    return this.collection(collection).has(serializeId(id));
  }

  async find(collection: string, filters: AdapterFilter[], options: AdapterFindOptions): Promise<AdapterFindResult> {
    return findInStore(this.store, collection, filters, options);
  }

  async save(collection: string, id: AdapterId, doc: Record<string, unknown>): Promise<Record<string, unknown>> {
    // Adapter save contract: return the *stored* document (read back from the
    // store), not the request body, so server-applied write transforms surface
    // identically across adapters. Memory applies no transforms, so the stored
    // value equals the written value here.
    const docs = this.collection(collection);
    const key = serializeId(id);
    docs.set(key, cloneValue(doc));
    return cloneValue(docs.get(key) as Record<string, unknown>);
  }

  async saveMany(
    collection: string,
    items: Array<{ id: AdapterId; data: Record<string, unknown> }>,
  ): Promise<Record<string, unknown>[]> {
    const docs = this.collection(collection);
    const saved: Record<string, unknown>[] = [];
    for (const item of items) {
      const key = serializeId(item.id);
      docs.set(key, cloneValue(item.data));
      // Read back the stored document to honor the save contract (see save()).
      saved.push(cloneValue(docs.get(key) as Record<string, unknown>));
    }
    return saved;
  }

  async delete(collection: string, id: AdapterId): Promise<Record<string, unknown> | undefined> {
    const docs = this.collection(collection);
    const key = serializeId(id);
    const existing = docs.get(key);
    if (!existing) return undefined;
    docs.delete(key);
    return cloneValue(existing);
  }

  async deleteMany(collection: string, filters: AdapterFilter[]): Promise<number> {
    const docs = this.collection(collection);
    let deleted = 0;
    for (const [serializedId, document] of docs.entries()) {
      if (filters.every((filter) => matchesFilter(document, filter))) {
        docs.delete(serializedId);
        deleted++;
      }
    }
    return deleted;
  }

  async runInTransaction<R>(fn: (tx: AdapterTx) => Promise<R>): Promise<R> {
    const previous = this.mutex;
    let release: (() => void) | undefined;
    this.mutex = new Promise((resolve) => {
      release = resolve;
    });

    await previous;
    const txStore = cloneStore(this.store);

    try {
      const result = await fn(txForStore(txStore));
      this.store = txStore;
      return result;
    } finally {
      release?.();
    }
  }

  async close(): Promise<void> {}
}
