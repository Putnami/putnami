import type { InferPrimitive, SchemaDescriptor } from '@putnami/runtime';
import type {
  CollectionDefinition,
  CollectionField,
  CollectionSchema,
  DocumentIdDefinition,
  FieldDefinition,
} from './collection-definition';

type Simplify<T> = { [K in keyof T]: T[K] } & {};

type InferCollectionField<F extends CollectionField> =
  F extends DocumentIdDefinition<infer P>
    ? InferPrimitive<P>
    : F extends FieldDefinition<infer P>
      ? InferPrimitive<P>
      : never;

type RequiredFieldKeys<S extends CollectionSchema> = {
  [K in keyof S]: S[K] extends DocumentIdDefinition
    ? K
    : S[K] extends FieldDefinition<infer P>
      ? P extends SchemaDescriptor
        ? P extends { optional: true }
          ? never
          : K
        : K
      : K;
}[keyof S];

type OptionalFieldKeys<S extends CollectionSchema> = {
  [K in keyof S]: S[K] extends DocumentIdDefinition
    ? never
    : S[K] extends FieldDefinition<infer P>
      ? P extends SchemaDescriptor
        ? P extends { optional: true }
          ? K
          : never
        : never
      : never;
}[keyof S];

type DocumentIdKeys<S extends CollectionSchema> = {
  [K in keyof S]: S[K] extends DocumentIdDefinition ? K : never;
}[keyof S];

type InferDocumentIdObject<S extends CollectionSchema> = Simplify<{
  [K in DocumentIdKeys<S>]: InferCollectionField<S[K]>;
}>;

type IsUnion<T, U = T> = T extends unknown ? ([U] extends [T] ? false : true) : never;

type InferDocumentIdFromSchema<S extends CollectionSchema> = [DocumentIdKeys<S>] extends [never]
  ? never
  : IsUnion<DocumentIdKeys<S>> extends true
    ? InferDocumentIdObject<S>
    : InferDocumentIdObject<S>[DocumentIdKeys<S>];

export type InferCollection<T extends CollectionDefinition> =
  T extends CollectionDefinition<infer S>
    ? Simplify<
        { [K in RequiredFieldKeys<S>]: InferCollectionField<S[K]> } & {
          [K in OptionalFieldKeys<S>]?: InferCollectionField<S[K]>;
        }
      >
    : never;

export type InferDocumentId<T extends CollectionDefinition> =
  T extends CollectionDefinition<infer S> ? InferDocumentIdFromSchema<S> : never;
