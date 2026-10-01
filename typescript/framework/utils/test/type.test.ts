import { describe, expect, it } from 'bun:test';
import { isFunction, isNullish, isObject, isPlainObject, isString, throwError } from '../src';

describe('type.utils', () => {
  describe('isString', () => {
    it('returns true for string primitives', () => {
      expect(isString('hello')).toBe(true);
    });

    it('returns true for String objects', () => {
      expect(isString(new String('hi'))).toBe(true);
    });

    it('returns false for numbers', () => {
      expect(isString(123)).toBe(false);
    });

    it('returns false for null', () => {
      expect(isString(null)).toBe(false);
    });

    it('returns false for undefined', () => {
      expect(isString(undefined)).toBe(false);
    });

    it('returns false for objects', () => {
      expect(isString({})).toBe(false);
    });
  });

  describe('isObject', () => {
    it('returns true for plain objects', () => {
      expect(isObject({})).toBe(true);
    });

    it('returns true for arrays', () => {
      expect(isObject([])).toBe(true);
    });

    it('returns true for functions', () => {
      expect(isObject(() => {})).toBe(true);
    });

    it('returns false for null', () => {
      expect(isObject(null)).toBe(false);
    });

    it('returns false for undefined', () => {
      expect(isObject(undefined)).toBe(false);
    });

    it('returns false for primitives', () => {
      expect(isObject('string')).toBe(false);
      expect(isObject(123)).toBe(false);
    });
  });

  describe('isPlainObject', () => {
    it('returns true for plain objects', () => {
      expect(isPlainObject({})).toBe(true);
      expect(isPlainObject({ a: 1 })).toBe(true);
    });

    it('returns false for arrays', () => {
      expect(isPlainObject([])).toBe(false);
    });

    it('returns false for dates', () => {
      expect(isPlainObject(new Date())).toBe(false);
    });

    it('returns false for null', () => {
      expect(isPlainObject(null)).toBe(false);
    });

    it('returns false for primitives', () => {
      expect(isPlainObject('string')).toBe(false);
    });
  });

  describe('isNullish', () => {
    it('returns true for null', () => {
      expect(isNullish(null)).toBe(true);
    });

    it('returns true for undefined', () => {
      expect(isNullish(undefined)).toBe(true);
    });

    it('returns false for 0', () => {
      expect(isNullish(0)).toBe(false);
    });

    it('returns false for empty string', () => {
      expect(isNullish('')).toBe(false);
    });

    it('returns false for false', () => {
      expect(isNullish(false)).toBe(false);
    });
  });

  describe('isFunction', () => {
    it('returns true for arrow functions', () => {
      expect(isFunction(() => {})).toBe(true);
    });

    it('returns true for regular functions', () => {
      expect(isFunction(() => {})).toBe(true);
    });

    it('returns true for classes', () => {
      expect(isFunction(class {})).toBe(true);
    });

    it('returns false for objects', () => {
      expect(isFunction({})).toBe(false);
    });

    it('returns false for null', () => {
      expect(isFunction(null)).toBe(false);
    });
  });

  describe('throwError / error', () => {
    it('throws an error with the given message', () => {
      expect(() => throwError('test message')).toThrow('test message');
    });
  });
});
