import { describe, expect, it } from 'bun:test';
import type { SchemaDefinition } from '@putnami/runtime';
import { ArrayOf, compileSchemaValidator, Default, Email, Int, MapOf, Optional, Uuid } from '@putnami/runtime';
import { validateSchema } from '../../../src/api/route/validate';

/** Materialise the emitted validator source into a callable. Test-only eval of generated code. */
type Materialized = (raw: unknown, fallback: (r: unknown) => unknown) => unknown;
// biome-ignore lint/security/noGlobalEval: executing generated validator source under test
const materialize = (source: string): Materialized => eval(`(${source})`);

/**
 * Assert the AOT validator (inline + fallback) behaves identically to the generic
 * route validator for a given input: same returned object on success, same thrown
 * error on failure.
 */
function expectEquivalent(schema: SchemaDefinition, coerce: boolean, input: unknown) {
  const compiled = compileSchemaValidator(schema, { coerce });
  expect(compiled.inlineable).toBe(true);
  const aot = materialize(compiled.source);
  const fallback = (raw: unknown) => validateSchema(schema, raw, { coerce, label: 'field' });

  let aotValue: unknown;
  let aotError: Error | undefined;
  try {
    aotValue = aot(input, fallback);
  } catch (e) {
    aotError = e as Error;
  }

  let genValue: unknown;
  let genError: Error | undefined;
  try {
    genValue = validateSchema(schema, input, { coerce, label: 'field' });
  } catch (e) {
    genError = e as Error;
  }

  if (genError) {
    expect(aotError).toBeDefined();
    expect(aotError?.message).toBe(genError.message);
  } else {
    expect(aotError).toBeUndefined();
    expect(aotValue).toEqual(genValue);
  }
}

describe('compileSchemaValidator — inlineable detection', () => {
  it('inlines scalars and Optional / Default / ArrayOf of scalars', () => {
    expect(compileSchemaValidator({ id: String }, { coerce: true }).inlineable).toBe(true);
    expect(compileSchemaValidator({ page: Int, limit: Int }, { coerce: true }).inlineable).toBe(true);
    expect(compileSchemaValidator({ name: String, age: Number, active: Boolean }, { coerce: false }).inlineable).toBe(
      true,
    );
    expect(compileSchemaValidator({ page: Optional(Int), q: Optional(String) }, { coerce: true }).inlineable).toBe(
      true,
    );
    expect(compileSchemaValidator({ limit: Default(Int, 10) }, { coerce: true }).inlineable).toBe(true);
    expect(compileSchemaValidator({ tags: ArrayOf(String), ids: ArrayOf(Int) }, { coerce: false }).inlineable).toBe(
      true,
    );
  });

  it('defers regex-constrained, nested, map and empty schemas', () => {
    expect(compileSchemaValidator({ id: Uuid }, { coerce: true }).inlineable).toBe(false);
    expect(compileSchemaValidator({ email: Email }, { coerce: false }).inlineable).toBe(false);
    expect(compileSchemaValidator({ id: Optional(Uuid) }, { coerce: true }).inlineable).toBe(false); // regex survives Optional
    expect(compileSchemaValidator({ ids: ArrayOf(Uuid) }, { coerce: false }).inlineable).toBe(false); // regex item
    expect(compileSchemaValidator({ m: MapOf(String, Int) }, { coerce: false }).inlineable).toBe(false);
    expect(compileSchemaValidator({ nested: { a: String } }, { coerce: false }).inlineable).toBe(false);
    expect(compileSchemaValidator({}, { coerce: true }).inlineable).toBe(false);
  });
});

describe('compileSchemaValidator — equivalence with validateSchema', () => {
  it('String params (coerce)', () => {
    const schema = { id: String };
    expectEquivalent(schema, true, { id: 'abc-123' });
    expectEquivalent(schema, true, {}); // missing → required error
    expectEquivalent(schema, true, { id: 123 }); // wrong type
    expectEquivalent(schema, true, { id: 'x', extra: 'dropped' }); // extra key dropped
    expectEquivalent(schema, true, null);
  });

  it('Int query (coerce)', () => {
    const schema = { page: Int, limit: Int };
    expectEquivalent(schema, true, { page: '2', limit: '20' });
    expectEquivalent(schema, true, { page: '2', limit: 'x' }); // NaN
    expectEquivalent(schema, true, { page: '2.5', limit: '3' }); // non-integer
    expectEquivalent(schema, true, { page: '2' }); // missing limit
    expectEquivalent(schema, true, { page: 2, limit: 20 }); // already numbers
  });

  it('Number (coerce)', () => {
    const schema = { score: Number };
    expectEquivalent(schema, true, { score: '2.5' });
    expectEquivalent(schema, true, { score: 'nope' });
    expectEquivalent(schema, true, { score: 7 });
  });

  it('Boolean (coerce)', () => {
    const schema = { active: Boolean };
    expectEquivalent(schema, true, { active: 'true' });
    expectEquivalent(schema, true, { active: 'false' });
    expectEquivalent(schema, true, { active: 'yes' }); // invalid
    expectEquivalent(schema, true, { active: true });
  });

  it('JSON body (no coerce) keeps types strict', () => {
    const schema = { name: String, age: Number };
    expectEquivalent(schema, false, { name: 'ada', age: 36 });
    expectEquivalent(schema, false, { name: 'ada', age: '36' }); // string age → error (no coerce)
    expectEquivalent(schema, false, { name: 42, age: 1 }); // wrong name type
  });

  it('Optional scalars (coerce)', () => {
    const schema = { page: Optional(Int), q: Optional(String) };
    expectEquivalent(schema, true, { page: '2', q: 'hi' });
    expectEquivalent(schema, true, {}); // both absent → empty result, no error
    expectEquivalent(schema, true, { page: '2' }); // q absent
    expectEquivalent(schema, true, { q: 'hi' }); // page absent
    expectEquivalent(schema, true, { page: 'x' }); // present-but-invalid → error
    expectEquivalent(schema, true, { page: null }); // null treated as absent
  });

  it('Default scalars', () => {
    const schema = { limit: Default(Int, 10), sort: Default(String, 'asc') };
    expectEquivalent(schema, true, {}); // both defaulted
    expectEquivalent(schema, true, { limit: '5' }); // override
    expectEquivalent(schema, true, { limit: '5', sort: 'desc' });
    expectEquivalent(schema, true, { limit: 'x' }); // invalid override → error
  });

  it('ArrayOf scalars (body, no coerce)', () => {
    const schema = { tags: ArrayOf(String), ids: ArrayOf(Int) };
    expectEquivalent(schema, false, { tags: ['a', 'b'], ids: [1, 2] });
    expectEquivalent(schema, false, { tags: [], ids: [] });
    expectEquivalent(schema, false, { tags: 'nope', ids: [1] }); // not an array → error
    expectEquivalent(schema, false, { tags: ['a'], ids: [1, 'x'] }); // bad item → error
    expectEquivalent(schema, false, { ids: [1] }); // required tags missing → error
  });
});
