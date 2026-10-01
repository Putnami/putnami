import { describe, expect, it } from 'bun:test';
import { isClassToken, isNamedToken, isTagSelector, named, tagged, tokenName } from '../../src/inject/token';

describe('token', () => {
  describe('named()', () => {
    it('should create a named token with the given name', () => {
      const token = named<string>('db-url');
      expect(token.name).toBe('db-url');
      expect(token.__brand).toBe('NamedToken');
    });

    it('should create distinct tokens for different names', () => {
      const a = named<string>('a');
      const b = named<string>('b');
      expect(a).not.toBe(b);
      expect(a.name).not.toBe(b.name);
    });
  });

  describe('tagged()', () => {
    it('should create a tag selector with the given tags', () => {
      const selector = tagged<unknown>('plugin');
      expect(selector.tags).toBe('plugin');
      expect(selector.__brand).toBe('TagSelector');
    });
  });

  describe('tokenName()', () => {
    it('should return named token representation', () => {
      expect(tokenName(named('my-config'))).toBe("named('my-config')");
    });

    it('should return tagged selector representation', () => {
      expect(tokenName(tagged('plugin'))).toBe("tagged('plugin')");
    });

    it('should return class name for class tokens', () => {
      class MyService {}
      expect(tokenName(MyService)).toBe('MyService');
    });

    it('should return symbol string for symbol tokens', () => {
      const sym = Symbol.for('test');
      expect(tokenName(sym)).toBe('Symbol(test)');
    });

    it('should return filter representation for FilterOptions with string tags', () => {
      // biome-ignore lint/suspicious/noExplicitAny: testing internal path
      expect(tokenName({ tags: 'plugin' } as any)).toBe("filter({ tags: 'plugin' })");
    });

    it('should return filter representation for FilterOptions with array tags', () => {
      // biome-ignore lint/suspicious/noExplicitAny: testing internal path
      expect(tokenName({ tags: ['a', 'b'] } as any)).toBe("filter({ tags: 'a, b' })");
    });
  });

  describe('type guards', () => {
    it('isNamedToken should detect named tokens', () => {
      expect(isNamedToken(named('x'))).toBe(true);
      expect(isNamedToken(tagged('x'))).toBe(false);
      expect(isNamedToken('string')).toBe(false);
      expect(isNamedToken(null)).toBe(false);
      expect(isNamedToken(class {})).toBe(false);
    });

    it('isTagSelector should detect tag selectors', () => {
      expect(isTagSelector(tagged('x'))).toBe(true);
      expect(isTagSelector(named('x'))).toBe(false);
      expect(isTagSelector('string')).toBe(false);
    });

    it('isClassToken should detect class tokens', () => {
      expect(isClassToken(class {})).toBe(true);
      expect(isClassToken(named('x'))).toBe(false);
      expect(isClassToken('string')).toBe(false);
      expect(isClassToken(42)).toBe(false);
    });
  });
});
