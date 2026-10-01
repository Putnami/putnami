import { describe, expect, it } from 'bun:test';
import { chunk, ensureArray, first, last, unique } from '../src';

describe('array.utils', () => {
  describe('ensureArray', () => {
    it('wraps a single value in an array', () => {
      expect(ensureArray('single')).toEqual(['single']);
    });

    it('returns array as-is', () => {
      expect(ensureArray(['a', 'b'])).toEqual(['a', 'b']);
    });

    it('wraps undefined in an array', () => {
      expect(ensureArray(undefined)).toEqual([undefined]);
    });

    it('returns empty array as-is', () => {
      expect(ensureArray([])).toEqual([]);
    });

    it('wraps numbers in an array', () => {
      expect(ensureArray(42)).toEqual([42]);
    });

    it('wraps objects in an array', () => {
      const obj = { a: 1 };
      expect(ensureArray(obj)).toEqual([obj]);
    });
  });

  describe('first', () => {
    it('returns the first element', () => {
      expect(first([1, 2, 3])).toBe(1);
    });

    it('returns undefined for empty array', () => {
      expect(first([])).toBeUndefined();
    });

    it('works with strings', () => {
      expect(first(['a', 'b'])).toBe('a');
    });
  });

  describe('last', () => {
    it('returns the last element', () => {
      expect(last([1, 2, 3])).toBe(3);
    });

    it('returns undefined for empty array', () => {
      expect(last([])).toBeUndefined();
    });

    it('works with single element', () => {
      expect(last(['only'])).toBe('only');
    });
  });

  describe('unique', () => {
    it('removes duplicate numbers', () => {
      expect(unique([1, 2, 2, 3, 1])).toEqual([1, 2, 3]);
    });

    it('removes duplicate strings', () => {
      expect(unique(['a', 'b', 'a'])).toEqual(['a', 'b']);
    });

    it('returns empty array for empty input', () => {
      expect(unique([])).toEqual([]);
    });

    it('preserves order of first occurrence', () => {
      expect(unique([3, 1, 2, 1, 3])).toEqual([3, 1, 2]);
    });
  });

  describe('chunk', () => {
    it('chunks array into specified size', () => {
      expect(chunk([1, 2, 3, 4, 5], 2)).toEqual([[1, 2], [3, 4], [5]]);
    });

    it('returns single chunk if array size equals chunk size', () => {
      expect(chunk([1, 2, 3], 3)).toEqual([[1, 2, 3]]);
    });

    it('returns empty array for empty input', () => {
      expect(chunk([], 2)).toEqual([]);
    });

    it('handles chunk size larger than array', () => {
      expect(chunk([1, 2], 5)).toEqual([[1, 2]]);
    });

    it('throws when size is zero', () => {
      expect(() => chunk([1, 2, 3], 0)).toThrow('chunk: size must be a positive integer, received 0');
    });

    it('throws when size is negative', () => {
      expect(() => chunk([1, 2, 3], -1)).toThrow('chunk: size must be a positive integer, received -1');
    });
  });
});
