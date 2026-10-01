import { FirestoreAdapter } from '../src/adapter/firestore.adapter';

type StoredDoc = Record<string, unknown>;
type WhereClause = { field: string; op: string; value: unknown };
type OrderClause = { field: string; direction: 'asc' | 'desc' };

function compareValues(left: unknown, right: unknown): number {
  if (left === right) return 0;
  if (left === undefined || left === null) return 1;
  if (right === undefined || right === null) return -1;
  if (typeof left === 'number' && typeof right === 'number') return left - right;
  if (typeof left === 'boolean' && typeof right === 'boolean') return Number(left) - Number(right);
  return String(left).localeCompare(String(right));
}

function matchesWhere(document: StoredDoc, clause: WhereClause): boolean {
  const value = document[clause.field];
  switch (clause.op) {
    case '==':
      return value === clause.value;
    case '!=':
      return value !== undefined && value !== clause.value;
    case '>':
      return value !== undefined && compareValues(value, clause.value) > 0;
    case '>=':
      return value !== undefined && compareValues(value, clause.value) >= 0;
    case '<':
      return value !== undefined && compareValues(value, clause.value) < 0;
    case '<=':
      return value !== undefined && compareValues(value, clause.value) <= 0;
    case 'in':
      return Array.isArray(clause.value) && clause.value.some((item) => item === value);
    case 'not-in':
      return value !== undefined && value !== null && Array.isArray(clause.value) && !clause.value.includes(value);
    case 'array-contains':
      return Array.isArray(value) && value.includes(clause.value);
    default:
      return false;
  }
}

export class FakeFirestoreClient {
  private store = new Map<string, Map<string, StoredDoc>>();
  readonly queryLimits: Array<number | undefined> = [];
  /** Number of `getAll` batched-read round-trips issued. */
  getAllCalls = 0;
  /** Number of single-document `get()` round-trips issued. */
  singleGetCalls = 0;

  /**
   * Optional server-side write transform. Real Firestore can mutate a document
   * on write (e.g. field sentinels), so the fake supports modeling that to keep
   * the shared adapter-save contract honest: the value the adapter returns must
   * reflect what was *stored*, not the request body. Defaults to identity.
   */
  constructor(private readonly serverWriteTransform: (data: StoredDoc) => StoredDoc = (data) => data) {}

  collection(name: string): FakeCollectionRef {
    return new FakeCollectionRef(this, name);
  }

  batch(): FakeBatch {
    return new FakeBatch(this);
  }

  async close(): Promise<void> {}

  set(collection: string, id: string, data: StoredDoc): void {
    const docs = this.store.get(collection) ?? new Map<string, StoredDoc>();
    docs.set(id, structuredClone(this.serverWriteTransform(data)));
    this.store.set(collection, docs);
  }

  delete(collection: string, id: string): void {
    this.store.get(collection)?.delete(id);
  }

  snapshot(collection: string, id: string): FakeDocSnapshot {
    const stored = this.store.get(collection)?.get(id);
    return new FakeDocSnapshot(this, collection, id, stored ? structuredClone(stored) : undefined);
  }

  /**
   * Batched multi-document read in a single round-trip, mirroring the real
   * `@google-cloud/firestore` `getAll`. Preserves the order of the supplied refs
   * so callers can zip the results back to their inputs positionally.
   */
  async getAll(...refs: FakeDocRef[]): Promise<FakeDocSnapshot[]> {
    this.getAllCalls += 1;
    return refs.map((ref) => this.snapshot(ref.collectionName, ref.id));
  }

  query(
    collection: string,
    whereClauses: WhereClause[],
    orderClauses: OrderClause[],
    limitValue?: number,
    startAfter?: FakeDocSnapshot,
  ): FakeDocSnapshot[] {
    this.queryLimits.push(limitValue);

    let docs = Array.from(this.store.get(collection)?.entries() ?? []).map(
      ([id, document]) => new FakeDocSnapshot(this, collection, id, structuredClone(document)),
    );

    docs = docs.filter((doc) => whereClauses.every((clause) => matchesWhere(doc.data() ?? {}, clause)));
    docs.sort((left, right) => compareSnapshots(left, right, orderClauses));

    if (startAfter) {
      docs = docs.filter((doc) => compareSnapshots(doc, startAfter, orderClauses) > 0);
    }

    if (limitValue !== undefined) {
      docs = docs.slice(0, limitValue);
    }

    return docs;
  }

  /**
   * Models Firestore's optimistic transactions closely enough for the shared
   * adapter contract: buffered writes are applied atomically on success, the
   * buffer is discarded when the body throws, and reads inside the body observe
   * earlier writes (read-your-writes), matching the document framework's
   * documented transaction semantics.
   */
  async runTransaction<R>(fn: (transaction: FakeTransaction) => Promise<R>): Promise<R> {
    const transaction = new FakeTransaction(this);
    const result = await fn(transaction);
    transaction.commit();
    return result;
  }
}

class FakeDocRef {
  constructor(
    private readonly client: FakeFirestoreClient,
    readonly collectionName: string,
    readonly id: string,
  ) {}

  async get(): Promise<FakeDocSnapshot> {
    this.client.singleGetCalls += 1;
    return this.client.snapshot(this.collectionName, this.id);
  }

  async set(data: StoredDoc): Promise<void> {
    this.client.set(this.collectionName, this.id, data);
  }

  async delete(): Promise<void> {
    this.client.delete(this.collectionName, this.id);
  }
}

class FakeDocSnapshot {
  readonly ref: FakeDocRef;

  constructor(
    client: FakeFirestoreClient,
    readonly collectionName: string,
    readonly id: string,
    private readonly document?: StoredDoc,
  ) {
    this.ref = new FakeDocRef(client, collectionName, id);
  }

  get exists(): boolean {
    return this.document !== undefined;
  }

  data(): StoredDoc | undefined {
    return this.document ? structuredClone(this.document) : undefined;
  }
}

function compareSnapshots(left: FakeDocSnapshot, right: FakeDocSnapshot, orderClauses: OrderClause[]): number {
  const leftData = left.data() ?? {};
  const rightData = right.data() ?? {};

  for (const order of orderClauses) {
    const compared = compareValues(leftData[order.field], rightData[order.field]);
    if (compared !== 0) {
      return order.direction === 'desc' ? compared * -1 : compared;
    }
  }

  return left.id.localeCompare(right.id);
}

class FakeQuery {
  constructor(
    protected readonly client: FakeFirestoreClient,
    protected readonly collectionName: string,
    protected readonly whereClauses: WhereClause[] = [],
    protected readonly orderClauses: OrderClause[] = [],
    protected readonly limitValue?: number,
    protected readonly startAfterSnapshot?: FakeDocSnapshot,
  ) {}

  where(field: string, op: string, value: unknown): FakeQuery {
    return new FakeQuery(
      this.client,
      this.collectionName,
      [...this.whereClauses, { field, op, value }],
      this.orderClauses,
      this.limitValue,
      this.startAfterSnapshot,
    );
  }

  orderBy(field: string, direction: 'asc' | 'desc' = 'asc'): FakeQuery {
    return new FakeQuery(
      this.client,
      this.collectionName,
      this.whereClauses,
      [...this.orderClauses, { field, direction }],
      this.limitValue,
      this.startAfterSnapshot,
    );
  }

  limit(limitValue: number): FakeQuery {
    return new FakeQuery(
      this.client,
      this.collectionName,
      this.whereClauses,
      this.orderClauses,
      limitValue,
      this.startAfterSnapshot,
    );
  }

  startAfter(snapshot: FakeDocSnapshot): FakeQuery {
    return new FakeQuery(
      this.client,
      this.collectionName,
      this.whereClauses,
      this.orderClauses,
      this.limitValue,
      snapshot,
    );
  }

  async get(): Promise<{ docs: FakeDocSnapshot[] }> {
    return {
      docs: this.client.query(
        this.collectionName,
        this.whereClauses,
        this.orderClauses,
        this.limitValue,
        this.startAfterSnapshot,
      ),
    };
  }
}

class FakeCollectionRef extends FakeQuery {
  doc(id: string): FakeDocRef {
    return new FakeDocRef(this.client, this.collectionName, id);
  }
}

class FakeBatch {
  private operations: Array<() => void> = [];

  constructor(private readonly client: FakeFirestoreClient) {}

  set(ref: FakeDocRef, data: StoredDoc): void {
    this.operations.push(() => this.client.set(ref.collectionName, ref.id, data));
  }

  delete(ref: FakeDocRef): void {
    this.operations.push(() => this.client.delete(ref.collectionName, ref.id));
  }

  async commit(): Promise<void> {
    for (const operation of this.operations) {
      operation();
    }
    this.operations = [];
  }
}

type BufferedWrite =
  | { type: 'set'; collection: string; id: string; data: StoredDoc }
  | { type: 'delete'; collection: string; id: string };

/**
 * Buffers writes against an overlay so the transaction body sees its own
 * mutations, then flushes them to the backing client when {@link commit} runs.
 */
class FakeTransaction {
  private readonly buffer = new Map<string, BufferedWrite>();

  constructor(private readonly client: FakeFirestoreClient) {}

  private key(collection: string, id: string): string {
    return JSON.stringify([collection, id]);
  }

  private bufferedSnapshot(ref: FakeDocRef): FakeDocSnapshot | undefined {
    const write = this.buffer.get(this.key(ref.collectionName, ref.id));
    if (!write) return undefined;
    return new FakeDocSnapshot(
      this.client,
      ref.collectionName,
      ref.id,
      write.type === 'set' ? structuredClone(write.data) : undefined,
    );
  }

  async get(ref: FakeDocRef): Promise<FakeDocSnapshot>;
  async get(query: FakeQuery): Promise<{ docs: FakeDocSnapshot[] }>;
  async get(target: FakeDocRef | FakeQuery): Promise<FakeDocSnapshot | { docs: FakeDocSnapshot[] }> {
    if (target instanceof FakeQuery) {
      return target.get();
    }
    return this.bufferedSnapshot(target) ?? target.get();
  }

  set(ref: FakeDocRef, data: StoredDoc): void {
    this.buffer.set(this.key(ref.collectionName, ref.id), {
      type: 'set',
      collection: ref.collectionName,
      id: ref.id,
      data: structuredClone(data),
    });
  }

  delete(ref: FakeDocRef): void {
    this.buffer.set(this.key(ref.collectionName, ref.id), {
      type: 'delete',
      collection: ref.collectionName,
      id: ref.id,
    });
  }

  commit(): void {
    for (const write of this.buffer.values()) {
      if (write.type === 'set') {
        this.client.set(write.collection, write.id, write.data);
      } else {
        this.client.delete(write.collection, write.id);
      }
    }
    this.buffer.clear();
  }
}

/**
 * Builds a {@link FirestoreAdapter} backed by an in-memory {@link FakeFirestoreClient},
 * bypassing the optional `@google-cloud/firestore` peer dependency. Returns the
 * adapter together with the fake so tests can assert on client-level behaviour
 * (e.g. issued query limits). Pass an existing client to share state.
 */
export function createFirestoreAdapter(client: FakeFirestoreClient = new FakeFirestoreClient()): {
  adapter: FirestoreAdapter;
  client: FakeFirestoreClient;
} {
  const adapter = new FirestoreAdapter('test', { projectId: 'putnami-document-tests' });
  (adapter as unknown as { clientPromise: Promise<FakeFirestoreClient> }).clientPromise = Promise.resolve(client);
  return { adapter, client };
}

/**
 * A {@link FakeFirestoreClient} whose server-side write transform stamps every
 * stored document with `serverStamp: true`. Used by the shared adapter-save
 * contract to verify that the Firestore adapter returns the *stored* document
 * (re-read after write) rather than echoing the request body.
 */
export function createServerTransformingFirestoreClient(): FakeFirestoreClient {
  return new FakeFirestoreClient((data) => ({ ...data, serverStamp: true }));
}
