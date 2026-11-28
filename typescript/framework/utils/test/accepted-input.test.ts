import { describe, expect, it } from 'bun:test';
import {
  ArrayOf,
  compileAcceptedInput,
  Default,
  Email,
  Int,
  MapOf,
  MaxLength,
  Min,
  MinLength,
  Optional,
  type SchemaDefinition,
  type SchemaDescriptor,
  type SchemaPrimitive,
  Uuid,
  validateSchema,
} from '../src/shared/schema';

const id = '550e8400-e29b-41d4-a716-446655440000';

/** Schemas with the inputs to check them on, valid and not. */
const cases: { schema: SchemaDefinition; coerce: boolean; inputs: unknown[] }[] = [
  {
    schema: { id: Uuid },
    coerce: true,
    inputs: [{ id }, { id: 'not-a-uuid' }, {}, { id: null }, { id: 7 }, null, 'text', [id]],
  },
  {
    schema: { page: Default(Int, 1), size: Optional(Min(1)), active: Boolean, q: Optional(String) },
    coerce: true,
    inputs: [
      { page: '3', size: '10', active: 'true', q: 'x' },
      { active: 'false' },
      { page: '1.5', active: 'true' },
      { page: 'abc', active: 'true' },
      { size: '0', active: 'true' },
      { active: 'yes' },
      { page: null, size: undefined, active: true },
      {},
    ],
  },
  {
    schema: {
      title: MaxLength(200),
      description: Default(String, ''),
      priority: Default(Int, 1),
      email: Optional(Email),
    },
    coerce: false,
    inputs: [
      { title: 'Write the report' },
      { title: 'Write', description: 'd', priority: 3, email: 'a@example.com' },
      { title: 'x'.repeat(201) },
      { title: 'Write', priority: '3' },
      { title: 'Write', priority: 1.5 },
      { title: 'Write', email: 'not-an-email' },
      { title: 'Write', extra: 'ignored' },
      { description: 'no title' },
      { title: 'Write', amount: Number.NaN },
    ],
  },
  {
    schema: { amount: Number, label: Default(String, null as unknown as string) },
    coerce: false,
    inputs: [{ amount: 1 }, { amount: Number.NaN }, { amount: '1' }, { amount: 2, label: 'l' }],
  },
];

describe('compileAcceptedInput', () => {
  for (const [index, { schema, coerce, inputs }] of cases.entries()) {
    it(`accepts what validateSchema accepts, with the same data, and nothing else (case ${index})`, () => {
      const accepted = compileAcceptedInput(schema, { coerce });
      expect(accepted).toBeDefined();
      for (const input of inputs) {
        const generic = validateSchema(schema, input, { coerce, label: 'field' });
        const data = accepted?.(input);
        if (generic.errors.length > 0) {
          expect(data).toBeUndefined();
        } else {
          // The same fields, in the same order, with the same values.
          expect(data).toEqual(generic.data);
          expect(Object.keys(data ?? {})).toEqual(Object.keys(generic.data));
        }
      }
    });
  }

  it('accepts the valid inputs it was built for', () => {
    expect(compileAcceptedInput({ id: Uuid }, { coerce: true })?.({ id })).toEqual({ id });
    expect(compileAcceptedInput({ page: Default(Int, 1) }, { coerce: true })?.({ page: '2' })).toEqual({ page: 2 });
  });

  it('reads a null, undefined or non-object input as validateSchema does', () => {
    const nonObjects: unknown[] = [null, undefined, 'text', 42, true, [], [id]];
    const optional = { q: Optional(String), page: Default(Int, 1) };
    const accepted = compileAcceptedInput(optional, { coerce: true });
    for (const input of nonObjects) {
      const generic = validateSchema(optional, input, { coerce: true });
      expect(generic.errors).toEqual([]);
      expect(accepted?.(input)).toEqual({ page: 1 });
      expect(accepted?.(input)).toEqual(generic.data);
    }

    const required = compileAcceptedInput({ id: Uuid }, { coerce: true });
    for (const input of nonObjects) {
      expect(validateSchema({ id: Uuid }, input, { coerce: true }).errors).not.toEqual([]);
      expect(required?.(input)).toBeUndefined();
    }
  });

  it('compiles no field of a base type other than string, number or boolean', () => {
    // A descriptor of another base type: `validateSchema` alone knows what it accepts.
    const ofBaseType = (baseType: string): SchemaDescriptor => ({ ...MinLength(1), baseType });
    expect(compileAcceptedInput({ meta: {} })).toBeUndefined();
    expect(compileAcceptedInput({ when: Date as unknown as SchemaPrimitive })).toBeUndefined();
    expect(compileAcceptedInput({ payload: ofBaseType('object') })).toBeUndefined();
    expect(compileAcceptedInput({ day: ofBaseType('date') })).toBeUndefined();
    expect(compileAcceptedInput({ id: Uuid, day: Optional(ofBaseType('date')) })).toBeUndefined();
    expect(compileAcceptedInput({ title: ofBaseType('string'), done: Boolean })).toBeDefined();
  });

  it('compiles no schema with an array, a map or a nested object', () => {
    expect(compileAcceptedInput({ tags: ArrayOf(String) })).toBeUndefined();
    expect(compileAcceptedInput({ prices: MapOf(String, Number) })).toBeUndefined();
    expect(compileAcceptedInput({ address: { city: String } })).toBeUndefined();
    expect(compileAcceptedInput({ id: Uuid, tags: ArrayOf(String) })).toBeUndefined();
  });
});
