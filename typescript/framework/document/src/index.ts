export { Collection, DocumentId, Field } from './collection';
export type {
  CollectionDefinition,
  CollectionOptions,
  IndexDefinition,
  IndexFieldDefinition,
  FieldDefinition,
  FieldOptions,
  DocumentIdDefinition,
  DocumentIdOptions,
  InferCollection,
  InferDocumentId,
} from './collection';

export { Repository } from './repository';
export type {
  Consistency,
  FindOptions,
  FindOrder,
  FindResult,
  QueryConditionValue,
  QueryFilterOperators,
  QueryFilters,
} from './repository';
export type { SaveOptions } from './repository/repository';

export { useBackend, closeBackend, closeAllBackends } from './factory';
export { DocumentConfig } from './config';
export { document, type DocumentPluginConfig } from './document.plugin';
export { runInTransaction } from './transaction';
export type { TransactionOptions } from './transaction';
export {
  DocumentError,
  DocumentErrorCode,
  TransactionNotSupported,
  IndexMissing,
  StrongConsistencyUnsupported,
  CompositeDocumentIdNotSupported,
} from './errors';
// Only the public observability types are part of the contract. The `record*`
// emitters are internal instrumentation called by repository.ts/transaction.ts
// and are intentionally not re-exported, so consumers cannot couple themselves
// to package-owned metric mutation helpers.
export type { DocumentMetrics, DocumentOperation } from './observability';

export {
  Optional,
  ArrayOf,
  MapOf,
  Stream,
  isStreamSchema,
  Uuid,
  Email,
  Int,
  Url,
  DateIso,
  Min,
  Max,
  MinLength,
  MaxLength,
  Pattern,
  OneOf,
  Constrained,
  Default,
  Env,
  Resolve,
  Sensitive,
  schema,
  isSchemaDescriptor,
  isNestedSchema,
  baseTypeName,
} from '@putnami/runtime';
export type {
  SchemaDescriptor,
  SchemaConstraint,
  SchemaPrimitive,
  NestedSchema,
  SchemaDefinition,
  StreamSchema,
  InferPrimitive,
  InferSchema,
} from '@putnami/runtime';
