import { describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/spectest';
import type { SchemaDefinition } from '../src/shared/schema/schema';
import { ArrayOf, Default, Int, Optional, Uuid } from '../src/shared/schema/schema';
import { validateSchema } from '../src/shared/schema/validate';
import { type AotFieldValidator, compileSchemaValidator } from '../src/shared/schema/validator-codegen';

/**
 * Parity suite for the AOT validator compiler: the compiled fast path
 * must produce byte-identical results to the interpreted
 * `validateSchema`, and must delegate to the fallback on every anomaly
 * so error behavior never diverges.
 *
 * The compiled `source` is materialized with `new Function` here the
 * way the build pipeline materializes it as bundled code.
 */

function materialize(source: string): AotFieldValidator {
  // biome-ignore lint/security/noGlobalEval: test-only materialization of build-time generated source
  return new Function(`return ${source}`)() as AotFieldValidator;
}

interface ParityOutcome {
  ok: boolean;
  data?: Record<string, unknown>;
  errors?: unknown;
  fallbackUsed?: boolean;
}

/** Runs the interpreted path with the throw-on-error contract consumers use. */
function interpreted(schema: SchemaDefinition, raw: unknown, coerce: boolean): ParityOutcome {
  const { data, errors } = validateSchema(schema, raw, { coerce });
  if (errors.length > 0) {
    return { ok: false, errors };
  }
  return { ok: true, data };
}

/** Runs the compiled path with a fallback wrapping the interpreted validator. */
function compiled(schema: SchemaDefinition, raw: unknown, coerce: boolean): ParityOutcome {
  const result = compileSchemaValidator(schema, { coerce });
  expect(result.inlineable).toBe(true);
  const validator = materialize(result.source);

  let fallbackUsed = false;
  try {
    const data = validator(raw, (r) => {
      fallbackUsed = true;
      const res = validateSchema(schema, r, { coerce });
      if (res.errors.length > 0) {
        throw new Error(JSON.stringify(res.errors));
      }
      return res.data;
    });
    return { ok: true, data, fallbackUsed };
  } catch (error) {
    return { ok: false, errors: JSON.parse((error as Error).message), fallbackUsed };
  }
}

function expectParity(schema: SchemaDefinition, raw: unknown, coerce: boolean, opts?: { fastPath?: boolean }) {
  const want = interpreted(schema, raw, coerce);
  const got = compiled(schema, raw, coerce);
  expect(got.ok).toBe(want.ok);
  if (want.ok) {
    expect(got.data).toEqual(want.data as Record<string, unknown>);
  } else {
    expect(got.errors).toEqual(JSON.parse(JSON.stringify(want.errors)));
    expect(got.fallbackUsed).toBe(true); // anomalies must delegate
  }
  if (opts?.fastPath) {
    expect(got.fallbackUsed).toBe(false); // valid input stays inline
  }
}

describe('compileSchemaValidator parity with validateSchema', () => {
  const scalars: SchemaDefinition = {
    name: String,
    age: Number,
    active: Boolean,
    rank: Int,
  };

  specTest(
    'matches on valid scalar input without touching the fallback',
    {
      feature: 'typescript/declarative-schema',
      requirement: 'compiled-parity',
      check: 'valid-input-agrees-inline',
    },
    () => {
      expectParity(scalars, { name: 'ada', age: 36.5, active: true, rank: 3 }, false, { fastPath: true });
    },
  );

  it('matches on missing required fields (delegates)', () => {
    expectParity(scalars, { name: 'ada' }, false);
  });

  specTest(
    'matches on wrong types (delegates)',
    {
      feature: 'typescript/declarative-schema',
      requirement: 'compiled-parity',
      check: 'rejections-agree-and-delegate',
    },
    () => {
      expectParity(scalars, { name: 42, age: 'x', active: 'maybe', rank: 1.5 }, false);
    },
  );

  it('matches on non-integer Int (delegates)', () => {
    expectParity(scalars, { name: 'ada', age: 1, active: false, rank: 2.5 }, false);
  });

  describe('coercion (params/query mode)', () => {
    specTest(
      'coerces numeric and boolean strings identically',
      {
        feature: 'typescript/declarative-schema',
        requirement: 'opt-in-coercion',
        check: 'enabled-coercion-converts-numeric-and-boolean-strings',
      },
      () => {
        expectParity(scalars, { name: 'ada', age: '36.5', active: 'true', rank: '3' }, true);
        // The interpreted verdict both paths agree on is the coerced one.
        const want = interpreted(scalars, { name: 'ada', age: '36.5', active: 'true', rank: '3' }, true);
        expect(want.data).toEqual({ name: 'ada', age: 36.5, active: true, rank: 3 });
      },
    );

    specTest(
      'matches on non-coercible strings (delegates)',
      {
        feature: 'typescript/declarative-schema',
        requirement: 'opt-in-coercion',
        check: 'an-uncoercible-string-is-still-a-type-mismatch',
      },
      () => {
        expectParity(scalars, { name: 'ada', age: 'abc', active: 'yes-ish', rank: '2.5' }, true);
      },
    );

    specTest(
      'does not coerce when coerce is off',
      {
        feature: 'typescript/declarative-schema',
        requirement: 'opt-in-coercion',
        check: 'no-coercion-unless-enabled',
      },
      () => {
        expectParity(scalars, { name: 'ada', age: '36.5', active: 'true', rank: '3' }, false);
        // With coercion off the same strings are type mismatches, not conversions.
        const want = interpreted(scalars, { name: 'ada', age: '36.5', active: 'true', rank: '3' }, false);
        expect(want.ok).toBe(false);
      },
    );
  });

  describe('Optional and Default', () => {
    const schema: SchemaDefinition = {
      nickname: Optional(String),
      level: Default(Int, 7),
      verbose: Default(Boolean, false),
    };

    it('matches when optional fields are absent', () => {
      expectParity(schema, {}, false, { fastPath: true });
    });

    it('matches when optional fields are present', () => {
      expectParity(schema, { nickname: 'lovelace', level: 2, verbose: true }, false, { fastPath: true });
    });

    specTest(
      'applies identical defaults',
      {
        feature: 'typescript/declarative-schema',
        requirement: 'compiled-parity',
        check: 'defaults-agree',
      },
      () => {
        const got = compiled(schema, {}, false);
        expect(got.data).toEqual({ level: 7, verbose: false });
      },
    );

    it('matches when an optional field has the wrong type (delegates)', () => {
      expectParity(schema, { nickname: 99 }, false);
    });
  });

  describe('ArrayOf', () => {
    const schema: SchemaDefinition = { tags: ArrayOf(String), scores: ArrayOf(Int) };

    it('matches on valid arrays without touching the fallback', () => {
      expectParity(schema, { tags: ['a', 'b'], scores: [1, 2] }, false, { fastPath: true });
    });

    it('matches on non-arrays and bad items (delegates)', () => {
      expectParity(schema, { tags: 'a', scores: [1, 'x'] }, false);
    });
  });

  it('extra unknown fields produce identical output', () => {
    expectParity(scalars, { name: 'ada', age: 1, active: true, rank: 1, extra: 'dropped?' }, false);
  });

  specTest(
    'refuses to inline regex-constrained types',
    {
      feature: 'typescript/declarative-schema',
      requirement: 'compiled-parity',
      check: 'unsupported-cases-are-delegated',
    },
    () => {
      const result = compileSchemaValidator({ id: Uuid }, { coerce: false });
      expect(result.inlineable).toBe(false);
      expect(result.source).toBe('');
    },
  );
});
