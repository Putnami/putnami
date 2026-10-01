import { describe, expect, it } from 'bun:test';
import { assertSafeIdentifier, isSafeIdentifier } from '../src/metadata/identifier';

describe('assertSafeIdentifier', () => {
  it('accepts plain identifiers', () => {
    expect(() => assertSafeIdentifier('id', 'ctx')).not.toThrow();
    expect(() => assertSafeIdentifier('created_at', 'ctx')).not.toThrow();
    expect(() => assertSafeIdentifier('_private', 'ctx')).not.toThrow();
    expect(() => assertSafeIdentifier('Col123', 'ctx')).not.toThrow();
  });

  it('rejects identifiers that could break out of SQL', () => {
    expect(() => assertSafeIdentifier('name" = name OR "1"="1', 'where column')).toThrow(/Unsafe SQL identifier/);
    expect(() => assertSafeIdentifier('a; DROP TABLE t', 'ctx')).toThrow(/Unsafe SQL identifier/);
    expect(() => assertSafeIdentifier('with space', 'ctx')).toThrow();
    expect(() => assertSafeIdentifier('1leading', 'ctx')).toThrow();
    expect(() => assertSafeIdentifier('', 'ctx')).toThrow();
  });

  it('includes the context in the error message', () => {
    expect(() => assertSafeIdentifier('bad-name', 'orderBy column')).toThrow(/orderBy column/);
  });
});

describe('isSafeIdentifier', () => {
  it('returns true for valid identifiers and false otherwise', () => {
    expect(isSafeIdentifier('valid_name')).toBe(true);
    expect(isSafeIdentifier('na me')).toBe(false);
    expect(isSafeIdentifier('"quoted"')).toBe(false);
  });
});
