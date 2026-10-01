import type { SchemaPrimitive } from '@putnami/runtime';

const COLLECTION_MARKER = 'putnami:collection' as const;
const FIELD_MARKER = 'putnami:document-field' as const;
const DOCUMENT_ID_MARKER = 'putnami:document-id' as const;

type SortDirection = 'asc' | 'desc';

export interface FieldOptions {
  /** Backend field name. Defaults to the property name. */
  readonly fieldName?: string;
  /** Default value for full-document validation. */
  readonly default?: unknown;
  /** Transform from entity value to stored value. */
  readonly toDocument?: (value: unknown) => unknown;
  /** Transform from stored value to entity value. */
  readonly fromDocument?: (value: unknown) => unknown;
}

export interface DocumentIdOptions extends FieldOptions {}

export interface FieldDefinition<P extends SchemaPrimitive = SchemaPrimitive> {
  readonly __field: typeof FIELD_MARKER;
  readonly type: P;
  readonly options: FieldOptions;
}

export interface DocumentIdDefinition<P extends SchemaPrimitive = SchemaPrimitive> {
  readonly __documentId: typeof DOCUMENT_ID_MARKER;
  readonly type: P;
  readonly options: DocumentIdOptions;
}

export type CollectionField = FieldDefinition | DocumentIdDefinition;
export type CollectionSchema = Record<string, CollectionField>;

export interface IndexFieldDefinition {
  readonly field: string;
  readonly direction?: SortDirection;
}

export interface IndexDefinition {
  readonly name?: string;
  /** Convenience shorthand for portable index coverage checks. */
  readonly fields?: readonly string[];
  /** Future-proof access-pattern metadata for partitioned stores. */
  readonly partition?: readonly string[];
  /** Future-proof sort metadata for partitioned stores. */
  readonly sort?: readonly (string | IndexFieldDefinition)[];
  /** Advisory-only in v1. */
  readonly unique?: boolean;
}

export interface CollectionOptions {
  /** Named store path under `document.<name>`. */
  readonly db?: string;
  /** Declared queryable access patterns. */
  readonly indexes?: readonly IndexDefinition[];
}

export interface CollectionDefinition<S extends CollectionSchema = CollectionSchema> {
  readonly __collection: typeof COLLECTION_MARKER;
  readonly collectionName: string;
  readonly schema: S;
  readonly options: CollectionOptions;
}

export function isFieldDefinition(value: unknown): value is FieldDefinition {
  return typeof value === 'object' && value !== null && (value as FieldDefinition).__field === FIELD_MARKER;
}

export function isDocumentIdDefinition(value: unknown): value is DocumentIdDefinition {
  return (
    typeof value === 'object' && value !== null && (value as DocumentIdDefinition).__documentId === DOCUMENT_ID_MARKER
  );
}

export function isCollectionDefinition(value: unknown): value is CollectionDefinition {
  return (
    typeof value === 'object' && value !== null && (value as CollectionDefinition).__collection === COLLECTION_MARKER
  );
}
