import type { SchemaPrimitive } from '@putnami/runtime';
import type {
  CollectionDefinition,
  CollectionOptions,
  CollectionSchema,
  DocumentIdDefinition,
  DocumentIdOptions,
  FieldDefinition,
  FieldOptions,
} from './collection-definition';

const COLLECTION_MARKER = 'putnami:collection' as const;
const FIELD_MARKER = 'putnami:document-field' as const;
const DOCUMENT_ID_MARKER = 'putnami:document-id' as const;

export function Field<P extends SchemaPrimitive>(type: P, options?: FieldOptions): FieldDefinition<P> {
  return {
    __field: FIELD_MARKER,
    type,
    options: options ?? {},
  };
}

export function DocumentId<P extends SchemaPrimitive>(type: P, options?: DocumentIdOptions): DocumentIdDefinition<P> {
  return {
    __documentId: DOCUMENT_ID_MARKER,
    type,
    options: options ?? {},
  };
}

export function Collection<S extends CollectionSchema>(
  collectionName: string,
  schema: S,
  options?: CollectionOptions,
): CollectionDefinition<S> {
  return {
    __collection: COLLECTION_MARKER,
    collectionName,
    schema,
    options: options ?? {},
  };
}
