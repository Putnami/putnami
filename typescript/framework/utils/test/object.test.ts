import { describe, expect, it } from 'bun:test';
import { getDeep, mergeDeep, omitUndefinedValues, sortObjectKeys } from '../src';

describe('object.utils', () => {
  describe('getDeep / deepGet', () => {
    const testObject = {
      user: {
        name: 'John',
        address: {
          city: 'Paris',
          zip: '75001',
        },
        tags: ['developer', 'tester'],
      },
      count: 42,
    };

    it('gets a top-level property', () => {
      expect(getDeep(testObject, 'count')).toBe(42);
    });

    it('gets a nested property using dot notation', () => {
      expect(getDeep(testObject, 'user.name')).toBe('John');
    });

    it('gets a deeply nested property', () => {
      expect(getDeep(testObject, 'user.address.city')).toBe('Paris');
    });

    it('gets an array element using array notation', () => {
      expect(getDeep(testObject, 'user.tags[0]')).toBe('developer');
    });

    it('gets an array element using bracket notation', () => {
      expect(getDeep(testObject, 'user.tags[1]')).toBe('tester');
    });

    it('returns undefined for non-existent path', () => {
      expect(getDeep(testObject, 'user.nonexistent')).toBeUndefined();
    });

    it('returns undefined for deeply non-existent path', () => {
      expect(getDeep(testObject, 'user.address.country.code')).toBeUndefined();
    });

    it('returns undefined for empty path', () => {
      expect(getDeep(testObject, '')).toBeUndefined();
    });

    it('handles null gracefully', () => {
      expect(getDeep(null, 'any.path')).toBeUndefined();
    });

    it('handles undefined gracefully', () => {
      expect(getDeep(undefined, 'any.path')).toBeUndefined();
    });

    it('does not traverse the prototype chain via __proto__', () => {
      expect(getDeep({}, '__proto__')).toBeUndefined();
      expect(getDeep({}, '__proto__.polluted')).toBeUndefined();
      expect(getDeep(testObject, 'user.__proto__')).toBeUndefined();
    });

    it('does not expose constructor or prototype internals', () => {
      expect(getDeep({}, 'constructor')).toBeUndefined();
      expect(getDeep({}, 'constructor.prototype')).toBeUndefined();
      expect(getDeep({}, 'prototype')).toBeUndefined();
    });

    it('returns undefined for inherited (non-own) properties', () => {
      const proto = { inherited: 'nope' };
      const child = Object.create(proto) as Record<string, unknown>;
      child.own = 'yes';

      expect(getDeep(child, 'own')).toBe('yes');
      expect(getDeep(child, 'inherited')).toBeUndefined();
      expect(getDeep(child, 'toString')).toBeUndefined();
    });

    it('reads own keys named like prototype members', () => {
      const obj = { constructor: { id: 7 } };
      expect(getDeep(obj, 'constructor.id')).toBe(7);
    });
  });

  describe('mergeDeep', () => {
    it('merges two flat objects', () => {
      const a = { x: 1, y: 2 };
      const b = { y: 3, z: 4 };
      const result = mergeDeep(a, b);

      expect(result).toEqual({ x: 1, y: 3, z: 4 });
    });

    it('merges nested objects', () => {
      const a = { user: { name: 'Alice', age: 30 } };
      const b = { user: { age: 31, city: 'Paris' } };
      const result = mergeDeep(a, b);

      expect(result).toEqual({
        user: { name: 'Alice', age: 31, city: 'Paris' },
      });
    });

    it('merges deeply nested objects', () => {
      const a = {
        config: {
          database: { host: 'localhost', port: 5432 },
          cache: { ttl: 60 },
        },
      };
      const b = {
        config: {
          database: { port: 3306, user: 'admin' },
          logging: { level: 'info' },
        },
      };
      const result = mergeDeep(a, b);

      expect(result).toEqual({
        config: {
          database: { host: 'localhost', port: 3306, user: 'admin' },
          cache: { ttl: 60 },
          logging: { level: 'info' },
        },
      });
    });

    it('does not mutate or share nested values from the inputs', () => {
      const defaults = {
        features: [{ name: 'search', enabled: true }],
        theme: { color: 'blue', size: 'md' },
      };
      const overrides = {
        features: [{ name: 'search', enabled: false }],
        theme: { color: 'red' },
      };
      const result = mergeDeep(defaults, overrides);

      expect(result).toEqual({
        features: [{ name: 'search', enabled: false }],
        theme: { color: 'red', size: 'md' },
      });
      expect(defaults).toEqual({
        features: [{ name: 'search', enabled: true }],
        theme: { color: 'blue', size: 'md' },
      });
      expect(overrides).toEqual({
        features: [{ name: 'search', enabled: false }],
        theme: { color: 'red' },
      });
      expect(result.theme).not.toBe(defaults.theme);
      expect(result.theme).not.toBe(overrides.theme);
      expect(result.features).not.toBe(overrides.features);
      expect(result.features[0]).not.toBe(overrides.features[0]);
    });

    it('tolerates a nullish base object', () => {
      const overrides = { theme: { color: 'red' } };

      expect(mergeDeep(undefined, overrides)).toEqual({ theme: { color: 'red' } });
      expect(mergeDeep(null, overrides)).toEqual({ theme: { color: 'red' } });
      // The overriding value is still copied, not shared.
      expect(mergeDeep(undefined, overrides).theme).not.toBe(overrides.theme);
    });

    it('overwrites arrays instead of merging them', () => {
      const a = { tags: ['a', 'b'] };
      const b = { tags: ['c', 'd'] };
      const result = mergeDeep(a, b);

      expect(result).toEqual({ tags: ['c', 'd'] });
    });

    it('overwrites primitives with objects', () => {
      const a = { value: 123 };
      const b = { value: { nested: true } };
      const result = mergeDeep(a, b);

      expect(result as unknown as { value: { nested: true } }).toEqual({ value: { nested: true } });
    });

    it('overwrites objects with primitives', () => {
      const a = { value: { nested: true } };
      const b = { value: 123 };
      const result = mergeDeep(a, b);

      expect(result as unknown as { value: number }).toEqual({ value: 123 });
    });

    it('handles null values', () => {
      const a = { x: null };
      const b = { y: 1 };
      const result = mergeDeep(a, b);

      expect(result).toEqual({ x: null, y: 1 });
    });

    it('handles undefined values', () => {
      const a = { x: undefined };
      const b = { y: 1 };
      const result = mergeDeep(a, b);

      expect(result).toEqual({ x: undefined, y: 1 });
    });

    it('merges empty objects', () => {
      const result = mergeDeep({}, {});
      expect(result).toEqual({});
    });

    it('preserves first object when second is empty', () => {
      const a = { x: 1, y: 2 };
      const result = mergeDeep(a, {});
      expect(result).toEqual({ x: 1, y: 2 });
    });

    it('uses second object when first is empty', () => {
      const b = { x: 1, y: 2 };
      const result = mergeDeep({}, b);
      expect(result).toEqual({ x: 1, y: 2 });
    });

    it('does not pollute Object.prototype via __proto__/constructor payloads', () => {
      const protoPayload = JSON.parse('{"__proto__":{"polluted":true}}');
      mergeDeep({}, protoPayload);
      expect(({} as Record<string, unknown>).polluted).toBeUndefined();

      const ctorPayload = JSON.parse('{"constructor":{"prototype":{"polluted":true}}}');
      const result = mergeDeep({ safe: 1 }, ctorPayload);
      expect(({} as Record<string, unknown>).polluted).toBeUndefined();
      // Dangerous keys are skipped; legitimate keys still merge.
      expect(result.safe).toBe(1);
    });
  });

  describe('sortObjectKeys / sortKeys', () => {
    it('sorts object keys alphabetically', () => {
      const result = sortObjectKeys({ z: 1, a: 2, m: 3 });
      const keys = Object.keys(result);
      expect(keys).toEqual(['a', 'm', 'z']);
    });

    it('returns undefined for falsy input', () => {
      expect(sortObjectKeys(undefined)).toBeUndefined();
    });

    it('preserves values while sorting keys', () => {
      const result = sortObjectKeys({ z: 'last', a: 'first' });
      expect(result).toEqual({ a: 'first', z: 'last' });
    });
  });

  describe('omitUndefinedValues / omitUndefined', () => {
    it('removes undefined values', () => {
      const result = omitUndefinedValues({ a: 1, b: undefined, c: 3 });
      expect(result).toEqual({ a: 1, c: 3 });
    });

    it('preserves null values', () => {
      const result = omitUndefinedValues({ a: null, b: undefined });
      expect(result).toEqual({ a: null });
    });

    it('preserves Date objects', () => {
      const date = new Date();
      const result = omitUndefinedValues({ date, value: undefined });
      expect(result.date).toBe(date);
    });

    it('recursively removes undefined from nested objects', () => {
      const result = omitUndefinedValues({
        a: 1,
        nested: { b: undefined, c: 2 },
      });
      expect(result).toEqual({ a: 1, nested: { c: 2 } });
    });

    it('strips undefined from deeply nested plain objects', () => {
      const result = omitUndefinedValues({
        a: { b: { c: undefined, d: 1 } },
      });
      expect(result).toEqual({ a: { b: { d: 1 } } });
    });

    it('preserves functions unchanged (identity + typeof)', () => {
      const fn = () => 42;
      const result = omitUndefinedValues({ fn, drop: undefined });
      expect(result.fn).toBe(fn);
      expect(typeof result.fn).toBe('function');
      expect(result.fn()).toBe(42);
    });

    it('preserves RegExp unchanged (identity + instanceof)', () => {
      const re = /abc/g;
      const result = omitUndefinedValues({ re, drop: undefined });
      expect(result.re).toBe(re);
      expect(result.re).toBeInstanceOf(RegExp);
      expect(result.re.source).toBe('abc');
    });

    it('preserves Map unchanged (identity + instanceof + entries)', () => {
      const map = new Map([['k', 'v']]);
      const result = omitUndefinedValues({ map, drop: undefined });
      expect(result.map).toBe(map);
      expect(result.map).toBeInstanceOf(Map);
      expect(result.map.get('k')).toBe('v');
    });

    it('preserves Set unchanged (identity + instanceof + members)', () => {
      const set = new Set([1, 2, 3]);
      const result = omitUndefinedValues({ set, drop: undefined });
      expect(result.set).toBe(set);
      expect(result.set).toBeInstanceOf(Set);
      expect(result.set.has(2)).toBe(true);
    });

    it('preserves class instances unchanged (identity + prototype + internal state)', () => {
      class Counter {
        constructor(public count: number) {}
        increment(): number {
          this.count += 1;

          return this.count;
        }
      }
      const counter = new Counter(5);
      const result = omitUndefinedValues({ counter, drop: undefined });
      expect(result.counter).toBe(counter);
      expect(result.counter).toBeInstanceOf(Counter);
      expect(result.counter.increment()).toBe(6);
    });

    it('preserves Object.create(null) by recursing without a prototype', () => {
      const bare = Object.create(null) as { keep: number; drop?: undefined };
      bare.keep = 1;
      bare.drop = undefined;
      const result = omitUndefinedValues({ bare });
      expect(result.bare).toEqual({ keep: 1 } as typeof bare);
      expect('drop' in result.bare).toBe(false);
    });
  });
});
