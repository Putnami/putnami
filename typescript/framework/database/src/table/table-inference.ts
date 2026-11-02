import type { InferPrimitive, SchemaDescriptor } from '@putnami/runtime';
import type { ColumnDefinition, KeyDefinition, TableDefinition, TableField, TableSchema } from './table-definition';

/** Infer the TypeScript type of a single TableField (Column or Key) */
export type InferField<F extends TableField> =
  F extends KeyDefinition<infer P>
    ? InferPrimitive<P>
    : F extends ColumnDefinition<infer P>
      ? InferPrimitive<P>
      : never;

/**
 * Whether a column holds an Optional() value: Optional() types its descriptor
 * `SchemaDescriptor<T | undefined>`, and its `optional` flag only as `boolean`.
 */
type IsOptionalColumn<P> = P extends SchemaDescriptor ? (undefined extends InferPrimitive<P> ? true : false) : false;

/** Property names of required fields (keys are always required, columns without Optional) */
type RequiredFieldKeys<S extends TableSchema> = {
  [K in keyof S]: S[K] extends KeyDefinition
    ? K
    : S[K] extends ColumnDefinition<infer P>
      ? IsOptionalColumn<P> extends true
        ? never
        : K
      : K;
}[keyof S];

/** Property names of optional fields (columns wrapped with Optional()) */
type OptionalFieldKeys<S extends TableSchema> = {
  [K in keyof S]: S[K] extends KeyDefinition
    ? never
    : S[K] extends ColumnDefinition<infer P>
      ? IsOptionalColumn<P> extends true
        ? K
        : never
      : never;
}[keyof S];

/** Flatten an intersection to a clean object type */
type Simplify<T> = { [K in keyof T]: T[K] } & {};

/** Infer the full entity type from a TableDefinition */
export type InferTable<T extends TableDefinition> =
  T extends TableDefinition<infer S>
    ? Simplify<{ [K in RequiredFieldKeys<S>]: InferField<S[K]> } & { [K in OptionalFieldKeys<S>]?: InferField<S[K]> }>
    : never;
