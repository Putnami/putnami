import { type InferConfig, useConfig } from '@putnami/runtime';
import type { AdapterFindResult, AdapterId, DocumentAdapter } from '../adapter/document.adapter';
import type { CollectionDefinition, InferCollection, InferDocumentId } from '../collection';
import { DocumentConfig } from '../config';
import {
  CompositeDocumentIdNotSupported,
  DocumentError,
  DocumentErrorCode,
  IndexMissing,
  StrongConsistencyUnsupported,
} from '../errors';
import { type DocumentHelper, documentHelper } from '../metadata';
import {
  type DocumentMetrics,
  type DocumentOperation,
  recordDocumentError,
  recordDocumentOp,
  recordSlowDocumentOp,
} from '../observability';
import { useBackend } from '../factory';
import {
  type Consistency,
  type FindOptions,
  type FindResult,
  normalizeOrderBy,
  parseQueryFilters,
  type QueryFilters,
} from './query';
import { useDocumentTransaction } from '../transaction/transaction';

type Entity<T extends CollectionDefinition> = InferCollection<T>;
const DELETE_MANY_PAGE_SIZE = 500;

export interface SaveOptions {
  /** Validate against the full schema. Defaults to `true` for replace writes and `false` for merge writes. */
  strict?: boolean;
  /** Merge into the existing document instead of replacing it. */
  merge?: boolean;
}

function normalizeFilters<T>(filters: QueryFilters<T> | Partial<T> | undefined): QueryFilters<T> {
  return (filters ?? {}) as QueryFilters<T>;
}

function normalizeLimit(value: unknown, fallback = 1000): number {
  // 0 is a legitimate "return no rows" limit, so it must be preserved rather
  // than collapsed into the default page size. Only an absent (`undefined`),
  // non-finite (`NaN`), or negative limit falls back to the default; every
  // adapter honors a normalized limit of 0 as zero rows (not unlimited).
  if (value === undefined || value === null) return fallback;
  const normalized = Math.floor(Number(value));
  return Number.isFinite(normalized) && normalized >= 0 ? normalized : fallback;
}

export class Repository<T extends CollectionDefinition> {
  protected helper: DocumentHelper<T>;
  private _adapter?: Promise<DocumentAdapter>;
  private _slowOperationThresholdMs?: number;

  constructor(collectionDef: T) {
    this.helper = documentHelper(collectionDef);
  }

  private useDocumentConfig(): InferConfig<typeof DocumentConfig> {
    const path = this.helper.db ? `document.${this.helper.db}` : undefined;
    return useConfig(DocumentConfig, { path });
  }

  private get slowOperationThresholdMs(): number {
    if (this._slowOperationThresholdMs === undefined) {
      try {
        this._slowOperationThresholdMs = this.useDocumentConfig().slowOperationThresholdMs;
      } catch {
        this._slowOperationThresholdMs = 0;
      }
    }
    return this._slowOperationThresholdMs;
  }

  private async adapter(): Promise<DocumentAdapter> {
    this._adapter ??= useBackend(this.helper.db);
    const adapter = await this._adapter;
    if (this.helper.hasCompositeId && !adapter.capabilities.compositeIds) {
      throw new CompositeDocumentIdNotSupported(
        `Backend "${this.useDocumentConfig().backend}" does not support composite document ids for collection "${this.helper.collectionName}"`,
      );
    }
    return adapter;
  }

  private async observe<R>(
    operation: DocumentOperation,
    action: () => Promise<R>,
    rowCount?: (result: R) => number,
  ): Promise<R> {
    const startedAt = Date.now();
    const collection = this.helper.collectionName;

    try {
      const result = await action();
      const metrics: DocumentMetrics = {
        operation,
        collection,
        duration: Date.now() - startedAt,
        rowCount: rowCount?.(result),
      };
      recordDocumentOp(metrics);
      if (this.slowOperationThresholdMs > 0) {
        recordSlowDocumentOp(metrics, this.slowOperationThresholdMs);
      }
      return result;
    } catch (error) {
      const metrics: DocumentMetrics = {
        operation,
        collection,
        duration: Date.now() - startedAt,
      };
      if (this.slowOperationThresholdMs > 0) {
        recordSlowDocumentOp(metrics, this.slowOperationThresholdMs);
      }
      recordDocumentError(operation, collection, metrics.duration, error);
      throw error;
    }
  }

  private assertProperty(property: string): void {
    if (!this.helper.hasProperty(property)) {
      throw new DocumentError(
        `Unknown property "${property}" for collection "${this.helper.collectionName}"`,
        DocumentErrorCode.Unknown,
      );
    }
  }

  private requireDocumentId(id: InferDocumentId<T> | Partial<Entity<T>>): AdapterId {
    const normalized = this.helper.normalizeDocumentId(id);
    if (normalized === undefined) {
      throw new DocumentError(
        `Missing document id for collection "${this.helper.collectionName}"`,
        DocumentErrorCode.MissingDocumentId,
      );
    }
    return normalized;
  }

  private assertIndexes(filters: QueryFilters<Entity<T>>, orderBy: string[]): void {
    const config = this.useDocumentConfig();
    // Firestore rejects uncovered queries only at runtime
    // (FAILED_PRECONDITION), so index enforcement defaults on for that
    // backend; memory has no index requirement, so it defaults off.
    const strictIndexes = config.strictIndexes ?? config.backend === 'firestore';
    if (!strictIndexes) return;

    const usedFields = new Set<string>();
    for (const property of Object.keys(filters as Record<string, unknown>)) {
      this.assertProperty(property);
      usedFields.add(this.helper.fieldName(property));
    }
    for (const field of orderBy) {
      usedFields.add(field);
    }

    if (usedFields.size === 0) return;

    const idFields = new Set(this.helper.idFieldNames);
    if (Array.from(usedFields).every((field) => idFields.has(field))) {
      return;
    }

    const declaredIndexes = this.helper.indexes.map((index) => {
      const explicitFields = index.fields ?? [];
      const partition = index.partition ?? [];
      const sort = (index.sort ?? []).map((item) => (typeof item === 'string' ? item : item.field));
      return new Set([...explicitFields, ...partition, ...sort].map((field) => this.helper.fieldName(field)));
    });

    const covered = declaredIndexes.some((indexFields) =>
      Array.from(usedFields).every((field) => indexFields.has(field)),
    );
    if (!covered) {
      throw new IndexMissing(
        `No declared index covers fields [${Array.from(usedFields).join(', ')}] on collection "${this.helper.collectionName}"`,
        Array.from(usedFields),
      );
    }
  }

  private async txOrAdapterFind(
    filters: QueryFilters<Entity<T>>,
    options: FindOptions<Entity<T>> = {},
  ): Promise<AdapterFindResult> {
    const adapter = await this.adapter();
    const tx = useDocumentTransaction(this.helper.db);
    const consistency: Consistency = options.consistency ?? 'strong';

    if (consistency === 'strong' && !adapter.capabilities.strongConsistency) {
      throw new StrongConsistencyUnsupported(
        `Backend "${this.useDocumentConfig().backend}" does not support strong consistency`,
      );
    }

    const parsedFilters = parseQueryFilters(
      filters,
      (property) => this.helper.fieldName(property),
      (property) => this.assertProperty(property),
    );
    const orderBy = normalizeOrderBy(
      options.orderBy,
      (property) => this.helper.fieldName(property),
      (property) => this.assertProperty(property),
      this.helper.idProperties,
    );

    this.assertIndexes(
      filters,
      orderBy.map((item) => item.field),
    );

    const findOptions = {
      limit: normalizeLimit(options.limit),
      cursor: options.cursor,
      orderBy,
      consistency,
    };

    if (tx) {
      return tx.find(this.helper.collectionName, parsedFilters, findOptions);
    }

    return adapter.find(this.helper.collectionName, parsedFilters, findOptions);
  }

  private async readRaw(id: AdapterId, consistency: Consistency): Promise<Record<string, unknown> | undefined> {
    const tx = useDocumentTransaction(this.helper.db);
    if (tx) {
      return tx.get(this.helper.collectionName, id);
    }
    const adapter = await this.adapter();
    return adapter.get(this.helper.collectionName, id, consistency);
  }

  get(id: InferDocumentId<T>, options?: { consistency?: Consistency }): Promise<Entity<T> | undefined> {
    return this.observe(
      'get',
      async () => {
        const raw = await this.readRaw(this.requireDocumentId(id), options?.consistency ?? 'strong');
        return raw ? this.helper.toEntity<Entity<T>>(raw) : undefined;
      },
      (result) => (result ? 1 : 0),
    );
  }

  exists(id: InferDocumentId<T>, options?: { consistency?: Consistency }): Promise<boolean> {
    return this.observe(
      'exists',
      async () => {
        const tx = useDocumentTransaction(this.helper.db);
        const normalizedId = this.requireDocumentId(id);
        if (tx) {
          return (await tx.get(this.helper.collectionName, normalizedId)) !== undefined;
        }
        const adapter = await this.adapter();
        const consistency = options?.consistency ?? 'strong';
        if (consistency === 'strong' && !adapter.capabilities.strongConsistency) {
          throw new StrongConsistencyUnsupported(
            `Backend "${this.useDocumentConfig().backend}" does not support strong consistency`,
          );
        }
        return adapter.exists(this.helper.collectionName, normalizedId, consistency);
      },
      (found) => (found ? 1 : 0),
    );
  }

  async findOne(
    filters: QueryFilters<Entity<T>> | Partial<Entity<T>>,
    options?: Omit<FindOptions<Entity<T>>, 'limit' | 'cursor'>,
  ): Promise<Entity<T> | undefined> {
    const result = await this.find(filters, { ...options, limit: 1 });
    return result.items[0];
  }

  find(
    filters: QueryFilters<Entity<T>> | Partial<Entity<T>> = {} as QueryFilters<Entity<T>>,
    options?: FindOptions<Entity<T>>,
  ): Promise<FindResult<Entity<T>>> {
    return this.observe(
      'find',
      async () => {
        const result = await this.txOrAdapterFind(normalizeFilters(filters), options);
        return {
          items: result.items.map((item) => this.helper.toEntity<Entity<T>>(item)),
          nextCursor: result.nextCursor,
        };
      },
      (result) => result.items.length,
    );
  }

  save(document: Partial<Entity<T>>, options?: SaveOptions): Promise<Entity<T>> {
    return this.observe(
      'save',
      async () => {
        if (!document || Object.keys(document as Record<string, unknown>).length === 0) {
          throw new DocumentError('Cannot save an empty document', DocumentErrorCode.EmptyDocument);
        }

        const id = this.requireDocumentId(document);
        const merge = options?.merge === true;
        const strict = options?.strict ?? !merge;
        let nextDocument = document as Record<string, unknown>;

        if (merge) {
          const existingRaw = await this.readRaw(id, 'strong');
          const existingEntity = existingRaw ? this.helper.toEntity<Record<string, unknown>>(existingRaw) : {};
          nextDocument = { ...existingEntity, ...(document as Record<string, unknown>) };
        }

        const errors = strict
          ? this.helper.validateFullDocument(nextDocument)
          : this.helper.validateDocument(nextDocument);
        if (errors.length > 0) {
          throw new DocumentError(
            `Validation failed: ${errors.map((error) => `${error.field}: ${error.message}`).join('; ')}`,
            DocumentErrorCode.ValidationError,
          );
        }

        const raw = this.helper.toDocument(nextDocument);
        const tx = useDocumentTransaction(this.helper.db);
        if (tx) {
          await tx.save(this.helper.collectionName, id, raw);
          return this.helper.toEntity<Entity<T>>(raw);
        }

        const adapter = await this.adapter();
        const saved = await adapter.save(this.helper.collectionName, id, raw);
        return this.helper.toEntity<Entity<T>>(saved);
      },
      () => 1,
    );
  }

  /** `saveMany()` performs replace writes with full validation in v1. */
  saveMany(documents: Partial<Entity<T>>[]): Promise<Entity<T>[]> {
    return this.observe(
      'saveMany',
      async () => {
        if (!documents || documents.length === 0) {
          throw new DocumentError('Cannot save an empty document array', DocumentErrorCode.EmptyDocuments);
        }

        const tx = useDocumentTransaction(this.helper.db);
        const normalized = documents.map((document) => {
          if (!document || Object.keys(document as Record<string, unknown>).length === 0) {
            throw new DocumentError('Cannot save an empty document', DocumentErrorCode.EmptyDocument);
          }
          const errors = this.helper.validateFullDocument(document as Record<string, unknown>);
          if (errors.length > 0) {
            throw new DocumentError(
              `Validation failed: ${errors.map((error) => `${error.field}: ${error.message}`).join('; ')}`,
              DocumentErrorCode.ValidationError,
            );
          }
          const id = this.requireDocumentId(document);
          return {
            id,
            data: this.helper.toDocument(document as Record<string, unknown>),
          };
        });

        if (tx) {
          for (const item of normalized) {
            await tx.save(this.helper.collectionName, item.id, item.data);
          }
          return normalized.map((item) => this.helper.toEntity<Entity<T>>(item.data));
        }

        const adapter = await this.adapter();
        const saved = await adapter.saveMany(this.helper.collectionName, normalized);
        return saved.map((item) => this.helper.toEntity<Entity<T>>(item));
      },
      (result) => result.length,
    );
  }

  delete(id: InferDocumentId<T>): Promise<{ success: boolean; item?: Entity<T> }> {
    return this.observe(
      'delete',
      async () => {
        const normalizedId = this.requireDocumentId(id);
        const tx = useDocumentTransaction(this.helper.db);
        if (tx) {
          const existing = await tx.get(this.helper.collectionName, normalizedId);
          if (!existing) return { success: false };
          await tx.delete(this.helper.collectionName, normalizedId);
          return {
            success: true,
            item: this.helper.toEntity<Entity<T>>(existing),
          };
        }

        const adapter = await this.adapter();
        const deleted = await adapter.delete(this.helper.collectionName, normalizedId);
        if (!deleted) {
          return { success: false };
        }
        return {
          success: true,
          item: this.helper.toEntity<Entity<T>>(deleted),
        };
      },
      (result) => (result.success ? 1 : 0),
    );
  }

  deleteMany(filters: QueryFilters<Entity<T>> | Partial<Entity<T>>): Promise<number> {
    return this.observe(
      'deleteMany',
      async () => {
        const normalizedFilters = normalizeFilters(filters);
        const parsedFilters = parseQueryFilters(
          normalizedFilters,
          (property) => this.helper.fieldName(property),
          (property) => this.assertProperty(property),
        );

        if (parsedFilters.length === 0) {
          throw new DocumentError('Cannot delete without filters for safety', DocumentErrorCode.DeleteWithoutFilters);
        }

        const tx = useDocumentTransaction(this.helper.db);
        if (tx) {
          // Collect every matching id by paging with the cursor *without* deleting,
          // then issue the deletes. Deleting while paging breaks on backends that
          // buffer transactional writes until commit (e.g. Firestore): a re-read
          // never observes the buffered deletes, so the same first page is returned
          // forever and the transaction is exhausted. Reading fully before writing
          // also respects Firestore's read-before-write transaction rule.
          const ids: AdapterId[] = [];
          let cursor: string | undefined;
          while (true) {
            const result = await this.txOrAdapterFind(normalizedFilters, { limit: DELETE_MANY_PAGE_SIZE, cursor });
            for (const item of result.items) {
              const id = this.helper.normalizeDocumentId(this.helper.toEntity<Record<string, unknown>>(item));
              if (id === undefined) {
                throw new DocumentError(
                  `Missing document id for collection "${this.helper.collectionName}"`,
                  DocumentErrorCode.MissingDocumentId,
                );
              }
              ids.push(id);
            }

            if (result.items.length < DELETE_MANY_PAGE_SIZE || !result.nextCursor) {
              break;
            }
            cursor = result.nextCursor;
          }

          for (const id of ids) {
            await tx.delete(this.helper.collectionName, id);
          }
          return ids.length;
        }

        const adapter = await this.adapter();
        return adapter.deleteMany(this.helper.collectionName, parsedFilters);
      },
      (count) => count,
    );
  }
}
