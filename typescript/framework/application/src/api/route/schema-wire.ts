import {
  Desc,
  type InferPrimitive,
  isSchemaDescriptor,
  type SchemaConstraint,
  type SchemaDescriptor,
  type SchemaPrimitive,
} from '@putnami/runtime';

/**
 * Wire-shape declarations a first-party contract needs and the validation
 * vocabulary does not carry: the width of an integer and the nullability of a
 * value. They are declarations about the wire, not extra input rules, so they
 * reject nothing that the underlying type already accepts.
 */

/** The closed set of integer widths a first-party contract can declare (ADR 0004). */
export type IntegerWidth = 'int32' | 'int64' | 'uint32' | 'uint64';

/** The width an integer field carries when none is declared (ADR 0004). */
export const DEFAULT_INTEGER_WIDTH: IntegerWidth = 'int64';

const INTEGER_WIDTHS: ReadonlySet<string> = new Set<IntegerWidth>(['int32', 'int64', 'uint32', 'uint64']);

/** Constraint name carrying a declared integer width. */
export const INTEGER_WIDTH_CONSTRAINT = 'intWidth';

/** Constraint name carrying declared nullability. */
export const NULLABLE_CONSTRAINT = 'nullable';

/**
 * Declare the wire width of an integer field. Compose it with `Int`:
 *
 * ```typescript
 * { sequence: Constrained(Int, IntWidth('uint64')) }
 * ```
 *
 * An `Int` without a declared width is `int64` (ADR 0004): the width is
 * declared or defaulted, never inferred from the transport. `int64` and
 * `uint64` reach a generated TypeScript client as `bigint`, `int32` and
 * `uint32` as `number`.
 */
export function IntWidth(width: IntegerWidth): SchemaDescriptor<number> {
  if (!INTEGER_WIDTHS.has(width)) {
    throw new Error(`IntWidth: ${JSON.stringify(width)} is not one of int32, int64, uint32, uint64`);
  }
  return withConstraint(liftPrimitive(Number), {
    name: INTEGER_WIDTH_CONSTRAINT,
    value: width,
    // A width is a wire declaration: the value is already an integer by `Int`,
    // and range enforcement belongs to the declared bounds, not here.
    validate: () => true,
    message: `must be a ${width}`,
  }) as SchemaDescriptor<number>;
}

/**
 * Declare that a value may be `null` on the wire, so the contract projects
 * `nullable: true` and a generated client types it `T | null`.
 *
 * This describes what the provider writes. On the input side the shared
 * validator treats an explicit `null` as an absent value, so a nullable input
 * field also needs `Optional` to be accepted when the caller sends `null`.
 */
export function Nullable<P extends SchemaPrimitive>(type: P): SchemaDescriptor<InferPrimitive<P> | null> {
  const base = isSchemaDescriptor(type) ? type : liftPrimitive(type);
  return withConstraint(base, {
    name: NULLABLE_CONSTRAINT,
    value: true,
    // Nullability widens the declared wire shape; it never rejects a value the
    // underlying type accepts.
    validate: () => true,
    message: 'may be null',
  }) as SchemaDescriptor<InferPrimitive<P> | null>;
}

function withConstraint(base: SchemaDescriptor, constraint: SchemaConstraint): SchemaDescriptor {
  return { ...base, constraints: [...(base.constraints ?? []), constraint] };
}

/**
 * `Desc` is the only exported way to turn a bare `String`/`Number`/`Boolean` or
 * a nested schema into a descriptor. Its empty description is dropped so the
 * projection sees no metadata the author did not declare.
 */
function liftPrimitive(type: SchemaPrimitive): SchemaDescriptor {
  const { description: _undeclared, ...lifted } = Desc('', type);
  return lifted as SchemaDescriptor;
}
