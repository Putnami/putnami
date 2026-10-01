import { describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/spectest';
import {
  ArrayOf,
  baseTypeName,
  Constrained,
  DateIso,
  Default,
  Desc,
  Email,
  Env,
  Int,
  isNestedSchema,
  isSchemaDescriptor,
  isStreamSchema,
  MapOf,
  Max,
  MaxLength,
  Min,
  MinLength,
  OneOf,
  Optional,
  Pattern,
  Resolve,
  Sensitive,
  Stream,
  schema,
  Url,
  Uuid,
  validateSchema,
} from '../src';

describe('schema utilities', () => {
  it('provides basic schema helpers', () => {
    const def = schema({ id: String });
    expect(def).toEqual({ id: String });

    expect(isSchemaDescriptor(Uuid)).toBe(true);
    expect(isSchemaDescriptor(String)).toBe(false);

    expect(isNestedSchema({ name: String })).toBe(true);
    expect(isNestedSchema({})).toBe(false);
    expect(isNestedSchema(String)).toBe(false);
    expect(isNestedSchema(Uuid)).toBe(false);
    expect(isNestedSchema(null)).toBe(false);

    expect(baseTypeName(String)).toBe('string');
    expect(baseTypeName(Number)).toBe('number');
    expect(baseTypeName(Boolean)).toBe('boolean');
    expect(baseTypeName(Uuid)).toBe('string');
    expect(baseTypeName({ nested: String })).toBe('object');
    expect(baseTypeName('unexpected' as unknown as typeof String)).toBe('unknown');
  });

  it('builds descriptor combinators', async () => {
    const optionalPrimitive = Optional(Number);
    expect(optionalPrimitive.optional).toBe(true);
    expect(optionalPrimitive.baseType).toBe('number');

    const optionalDescriptor = Optional(Email);
    expect(optionalDescriptor.optional).toBe(true);
    expect(optionalDescriptor.constraints).toEqual(Email.constraints);

    const array = ArrayOf(String);
    expect(array.array).toBe(true);
    expect(array.baseType).toBe('array');
    expect(array.items).toBe(String);

    const map = MapOf(String, Number);
    expect(map.map).toBe(true);
    expect(map.baseType).toBe('map');
    expect(map.mapKey).toBe(String);
    expect(map.mapValue).toBe(Number);

    const streamed = Stream({ id: String });
    expect(isStreamSchema(streamed)).toBe(true);
    expect(isStreamSchema({})).toBe(false);

    const withDefault = Default(String, 'fallback');
    expect(withDefault.default).toBe('fallback');

    const withEnv = Env('API_TOKEN', String);
    expect(withEnv.env).toBe('API_TOKEN');

    const resolver = async () => 'secret';
    const withResolve = Resolve(resolver, String);
    expect(await withResolve.resolve?.()).toBe('secret');

    const sensitive = Sensitive(String);
    expect(sensitive.sensitive).toBe(true);

    const described = Desc('username field', String);
    expect(described.description).toBe('username field');

    // Desc() now wraps nested object schemas — the description rides
    // alongside the nested shape so config extractors and OpenAPI emitters
    // can surface it without losing the nested fields.
    const describedNested = Desc('nested block', { inner: String });
    expect(describedNested.description).toBe('nested block');
    expect(describedNested.baseType).toBe('object');
    expect(describedNested.schema).toEqual({ inner: String });
  });

  it('validates built-in constraints and constrained composition', () => {
    expect(Uuid.constraints?.[0]?.validate('550e8400-e29b-41d4-a716-446655440000')).toBe(true);
    expect(Uuid.constraints?.[0]?.validate('not-a-uuid')).toBe(false);

    // Uuid accepts any RFC 4122 version/variant, not only v4 (see docblock).
    // v4 (random), v7 (time-ordered) and the nil UUID must all validate.
    expect(Uuid.constraints?.[0]?.validate('f47ac10b-58cc-4372-a567-0e02b2c3d479')).toBe(true); // v4
    expect(Uuid.constraints?.[0]?.validate('018f9c8e-1b7a-7c3e-9b0d-2a1c4e6f8a0b')).toBe(true); // v7
    expect(Uuid.constraints?.[0]?.validate('00000000-0000-0000-0000-000000000000')).toBe(true); // nil

    expect(Email.constraints?.[0]?.validate('user@example.com')).toBe(true);
    expect(Email.constraints?.[0]?.validate('bad-email')).toBe(false);

    expect(Int.constraints?.[0]?.validate(12)).toBe(true);
    expect(Int.constraints?.[0]?.validate(12.2)).toBe(false);

    expect(Url.constraints?.[0]?.validate('https://putnami.dev')).toBe(true);
    expect(Url.constraints?.[0]?.validate('not-a-url')).toBe(false);

    expect(DateIso.constraints?.[0]?.validate('2024-01-15')).toBe(true);
    expect(DateIso.constraints?.[0]?.validate('15/01/2024')).toBe(false);

    const min = Min(5);
    expect(min.constraints?.[0]?.validate(5)).toBe(true);
    expect(min.constraints?.[0]?.validate(4)).toBe(false);

    const max = Max(5);
    expect(max.constraints?.[0]?.validate(5)).toBe(true);
    expect(max.constraints?.[0]?.validate(6)).toBe(false);

    const minLength = MinLength(2);
    expect(minLength.constraints?.[0]?.validate('ab')).toBe(true);
    expect(minLength.constraints?.[0]?.validate('a')).toBe(false);

    const maxLength = MaxLength(2);
    expect(maxLength.constraints?.[0]?.validate('ab')).toBe(true);
    expect(maxLength.constraints?.[0]?.validate('abc')).toBe(false);

    const pattern = Pattern(/^X-\d+$/);
    expect(pattern.constraints?.[0]?.validate('X-42')).toBe(true);
    expect(pattern.constraints?.[0]?.validate('Y-42')).toBe(false);

    const oneOf = OneOf('dev', 'prod');
    expect(oneOf.constraints?.[0]?.validate('dev')).toBe(true);
    expect(oneOf.constraints?.[0]?.validate('test')).toBe(false);

    const constrained = Constrained(MinLength(3), MaxLength(5));
    expect(constrained.constraints?.length).toBe(2);
    expect(() => Constrained()).toThrow('Constrained() requires at least one descriptor');
  });

  specTest(
    'Pattern validation is stable across repeated calls for stateful g/y flags',
    {
      feature: 'typescript/declarative-schema',
      requirement: 'stateless-constraints',
      check: 'repeated-validation-yields-the-same-verdict',
    },
    () => {
      // A `/g` (or `/y`) RegExp advances lastIndex on each test(), so validating
      // the same value twice on one shared descriptor would flip pass -> fail.
      const globalPattern = Pattern(/^X-\d+$/g);
      const validate = globalPattern.constraints?.[0]?.validate;
      expect(validate?.('X-42')).toBe(true);
      expect(validate?.('X-42')).toBe(true);
      expect(validate?.('X-42')).toBe(true);
      expect(validate?.('Y-42')).toBe(false);
      expect(validate?.('Y-42')).toBe(false);

      const stickyPattern = Pattern(/^X-\d+$/y);
      const stickyValidate = stickyPattern.constraints?.[0]?.validate;
      expect(stickyValidate?.('X-7')).toBe(true);
      expect(stickyValidate?.('X-7')).toBe(true);
    },
  );

  specTest(
    'Pattern does not mutate the caller RegExp',
    {
      feature: 'typescript/declarative-schema',
      requirement: 'stateless-constraints',
      check: 'a-constraint-never-mutates-caller-values',
    },
    () => {
      const source = /^X-\d+$/g;
      Pattern(source).constraints?.[0]?.validate('X-42');
      // The caller's own RegExp is untouched; its lastIndex only advances if the
      // caller uses it, never as a side effect of building the descriptor.
      expect(source.lastIndex).toBe(0);
    },
  );
});

describe('validateSchema', () => {
  specTest(
    'returns required errors when a required field is absent',
    {
      feature: 'typescript/declarative-schema',
      requirement: 'required-and-default',
      check: 'a-missing-required-field-is-an-error',
    },
    () => {
      const result = validateSchema(schema({ name: String }), null);
      expect(result.data).toEqual({});
      expect(result.errors).toEqual([{ field: 'field.name', message: 'field.name is required' }]);

      // The same field missing from an otherwise valid object is equally an error.
      const missing = validateSchema(schema({ name: String }), {});
      expect(missing.errors).toEqual([{ field: 'field.name', message: 'field.name is required' }]);
    },
  );

  specTest(
    'validates, coerces, and applies defaults across nested structures',
    {
      feature: 'typescript/declarative-schema',
      requirement: 'required-and-default',
      check: 'a-default-fills-absence-and-is-present-in-output',
    },
    () => {
      const def = schema({
        id: Uuid,
        retries: Default(Number, 3),
        enabled: Boolean,
        disabled: Boolean,
        tags: ArrayOf(String),
        metrics: MapOf(String, Number),
        profile: {
          email: Email,
          age: Optional(Int),
          city: Default(String, 'Paris'),
        },
      });

      const result = validateSchema(
        def,
        {
          id: '550e8400-e29b-41d4-a716-446655440000',
          enabled: 'true',
          disabled: 'false',
          tags: ['a', 'b'],
          metrics: { ok: '10', other: 5 },
          profile: {
            email: 'user@example.com',
          },
        },
        { coerce: true, label: 'body' },
      );

      expect(result.errors).toEqual([]);
      expect(result.data).toEqual({
        id: '550e8400-e29b-41d4-a716-446655440000',
        retries: 3,
        enabled: true,
        disabled: false,
        tags: ['a', 'b'],
        metrics: { ok: 10, other: 5 },
        profile: {
          email: 'user@example.com',
          city: 'Paris',
        },
      });
    },
  );

  specTest(
    'reports type errors for array/map/nested mismatches',
    {
      feature: 'typescript/declarative-schema',
      requirement: 'nested-and-collections',
      check: 'a-wrong-container-type-is-rejected',
    },
    () => {
      const def = schema({
        arr: ArrayOf(String),
        map: MapOf(String, Number),
        nested: { key: String },
      });

      const result = validateSchema(
        def,
        {
          arr: 'not-an-array',
          map: [],
          nested: 123,
        },
        { label: 'query' },
      );

      expect(result.errors.map((e) => e.message)).toEqual([
        'query.arr must be an array',
        'query.map must be an object (map)',
        'query.nested must be an object',
      ]);
    },
  );

  specTest(
    'reports the failing path inside arrays, maps, and nested objects',
    {
      feature: 'typescript/declarative-schema',
      requirement: 'nested-and-collections',
      check: 'the-failing-path-is-reported',
    },
    () => {
      const def = schema({
        ints: ArrayOf(Int),
        labels: MapOf(String, Number),
        profile: { email: Email },
      });

      const result = validateSchema(
        def,
        {
          ints: [1, 'bad'],
          labels: { ok: 1, bad: 'x' },
          profile: { email: 'not-an-email' },
        },
        { label: 'x' },
      );

      expect(result.errors.map((e) => e.field).sort()).toEqual(['x.ints[1]', 'x.labels[bad]', 'x.profile.email']);
    },
  );

  specTest(
    'collects constraint errors and keeps valid values',
    {
      feature: 'typescript/declarative-schema',
      requirement: 'type-enforcement',
      check: 'a-type-mismatch-is-a-field-level-error',
    },
    () => {
      const customDescriptor = {
        __schema: 'putnami:schema',
        baseType: 'custom',
      } as const;

      const def = schema({
        secret: Sensitive(String),
        count: Number,
        ints: ArrayOf(Int),
        labels: MapOf(String, Number),
        variant: OneOf('a', 'b'),
        token: Pattern(/^X-\d+$/),
        minValue: Min(2),
        maxValue: Max(5),
        shortText: MaxLength(3),
        longText: MinLength(2),
        date: DateIso,
        site: Url,
        anyValue: customDescriptor,
      });

      const result = validateSchema(
        def,
        {
          secret: 42,
          count: 'oops',
          ints: [1, 'bad'],
          labels: { ok: 1, bad: 'x' },
          variant: 'z',
          token: 'bad',
          minValue: 1,
          maxValue: 6,
          shortText: 'abcd',
          longText: 'x',
          date: '2024/01/15',
          site: 'not-a-url',
          anyValue: { nested: true },
        },
        { label: 'payload' },
      );

      expect(result.errors.some((e) => e.message === 'payload.secret must be of type string')).toBe(true);
      expect(result.errors.some((e) => e.message === 'payload.count must be of type number')).toBe(true);
      expect(result.errors.some((e) => e.message === 'payload.ints[1] must be of type number')).toBe(true);
      expect(result.errors.some((e) => e.message === 'payload.labels[bad] must be of type number')).toBe(true);
      expect(result.errors.some((e) => e.message.includes('must be one of: a, b'))).toBe(true);
      expect(result.errors.some((e) => e.message.includes('must match pattern'))).toBe(true);
      expect(result.errors.some((e) => e.message.includes('must be >= 2'))).toBe(true);
      expect(result.errors.some((e) => e.message.includes('must be <= 5'))).toBe(true);
      expect(result.errors.some((e) => e.message.includes('must have length <= 3'))).toBe(true);
      expect(result.errors.some((e) => e.message.includes('must have length >= 2'))).toBe(true);
      expect(result.errors.some((e) => e.message.includes('must be a valid ISO 8601 date string'))).toBe(true);
      expect(result.errors.some((e) => e.message.includes('must be a valid URL'))).toBe(true);

      expect(result.data['ints']).toEqual([1, undefined]);
      expect(result.data['labels']).toEqual({ ok: 1 });
      expect(result.data['anyValue']).toEqual({ nested: true });
      expect(result.data['count']).toBeUndefined();
      expect(result.data['secret']).toBeUndefined();
    },
  );

  specTest(
    'keeps uncoercible values as-is and reports type mismatch',
    {
      feature: 'typescript/declarative-schema',
      requirement: 'type-enforcement',
      check: 'an-invalid-value-is-kept-as-authored-never-replaced',
    },
    () => {
      const def = schema({
        amount: Number,
        enabled: Boolean,
      });

      const input = { amount: 'NaN-value', enabled: 'not-bool' };
      const result = validateSchema(def, input, { coerce: true, label: 'params' });
      expect(result.errors).toEqual([
        { field: 'params.amount', message: 'params.amount must be of type number' },
        { field: 'params.enabled', message: 'params.enabled must be of type boolean' },
      ]);
      // The invalid value is dropped from the output, never replaced by a
      // fabricated one — and the caller's own object keeps what was authored.
      expect(result.data).toEqual({});
      expect(input).toEqual({ amount: 'NaN-value', enabled: 'not-bool' });
    },
  );
});
