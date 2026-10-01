import type { Consistency } from '../repository/query';

export type AdapterId = string | number | Record<string, string | number>;

export type AdapterOp = 'eq' | 'ne' | 'gt' | 'gte' | 'lt' | 'lte' | 'in' | 'notIn' | 'contains' | 'exists';

export interface AdapterFilter {
  field: string;
  op: AdapterOp;
  value: unknown;
}

export interface AdapterOrderBy {
  field: string;
  direction: 'asc' | 'desc';
}

export interface AdapterFindOptions {
  limit: number;
  cursor?: string;
  orderBy: AdapterOrderBy[];
  consistency: Consistency;
}

export interface AdapterFindResult {
  items: Record<string, unknown>[];
  nextCursor?: string;
}

export interface AdapterCapabilities {
  strongConsistency: boolean;
  eventualConsistency: boolean;
  transactions: boolean;
  contains: boolean;
  exists: boolean;
  compositeIds: boolean;
  strictIndexes: boolean;
}

export interface AdapterTx {
  get(collection: string, id: AdapterId): Promise<Record<string, unknown> | undefined>;
  find(collection: string, filters: AdapterFilter[], options: AdapterFindOptions): Promise<AdapterFindResult>;
  save(collection: string, id: AdapterId, doc: Record<string, unknown>): Promise<void>;
  delete(collection: string, id: AdapterId): Promise<void>;
}

export interface DocumentAdapter {
  readonly capabilities: AdapterCapabilities;
  get(collection: string, id: AdapterId, consistency: Consistency): Promise<Record<string, unknown> | undefined>;
  exists(collection: string, id: AdapterId, consistency: Consistency): Promise<boolean>;
  find(collection: string, filters: AdapterFilter[], options: AdapterFindOptions): Promise<AdapterFindResult>;
  save(collection: string, id: AdapterId, doc: Record<string, unknown>): Promise<Record<string, unknown>>;
  saveMany(
    collection: string,
    items: Array<{ id: AdapterId; data: Record<string, unknown> }>,
  ): Promise<Record<string, unknown>[]>;
  delete(collection: string, id: AdapterId): Promise<Record<string, unknown> | undefined>;
  deleteMany(collection: string, filters: AdapterFilter[]): Promise<number>;
  runInTransaction<R>(fn: (tx: AdapterTx) => Promise<R>): Promise<R>;
  close(): Promise<void>;
}
