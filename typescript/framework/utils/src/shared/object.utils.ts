/**
 * Object Utility Functions
 *
 * Browser and SSR compatible object manipulation utilities.
 * @module @putnami/utils
 */

/**
 * Retrieves a deeply nested value from an object using dot notation or array path.
 *
 * Supports multiple path formats:
 * - Dot notation: `'user.address.city'`
 * - Array notation: `['user', 'address', 'city']`
 * - Mixed notation: `'user.tags[0]'`
 *
 * @param object - The source object to query
 * @param path - The path to the property (dot notation string or array of keys)
 * @returns The value at the specified path, or `undefined` if not found
 *
 * @example
 * ```typescript
 * const user = { profile: { name: 'Alice', tags: ['admin', 'user'] } };
 *
 * getDeep(user, 'profile.name');      // 'Alice'
 * getDeep(user, 'profile.tags[0]');   // 'admin'
 * getDeep(user, ['profile', 'name']); // 'Alice'
 * getDeep(user, 'invalid.path');      // undefined
 * ```
 */
export function getDeep(object: unknown, path: string | (string | number)[]): unknown {
  if (!path) {
    return undefined;
  }
  const resolvePath = (path: string | (string | number)[]): (string | number)[] => {
    if (typeof path === 'string') {
      return path
        .replace(/\[(\w+)]/g, '.$1')
        .split('.')
        .filter(Boolean);
    }
    if (Array.isArray(path)) {
      return path;
    }

    return [path];
  };

  const elems = resolvePath(path);
  let result: unknown = object;

  for (const key of elems) {
    if (result === null || result === undefined) {
      return undefined;
    }
    if (typeof key === 'number' && Array.isArray(result)) {
      result = result[key];
    } else if (typeof result === 'object' && typeof key === 'string') {
      // Restrict to own properties so user-supplied paths cannot walk the
      // prototype chain: `__proto__`/`constructor`/`prototype` would otherwise
      // return prototype objects (e.g. Object.prototype).
      if (!Object.hasOwn(result, key)) {
        return undefined;
      }
      result = (result as Record<string, unknown>)[key];
    }
  }

  return result;
}

/**
 * Recursively merges two objects, with properties from the second object
 * taking precedence over the first. Arrays are overwritten, not merged.
 *
 * @template A - Type of the first object
 * @template B - Type of the second object
 * @param a - The base object
 * @param b - The object to merge into the base
 * @returns A new object with deeply merged properties. The input objects are not mutated.
 *
 * @example
 * ```typescript
 * const defaults = { theme: { color: 'blue', size: 'md' } };
 * const overrides = { theme: { color: 'red' } };
 *
 * mergeDeep(defaults, overrides);
 * // { theme: { color: 'red', size: 'md' } }
 * ```
 */
export function mergeDeep<A, B>(a: A, b: B): A & B {
  const isMergeableObject = (obj: unknown): obj is Record<string, unknown> => {
    if (!obj || typeof obj !== 'object' || Array.isArray(obj)) {
      return false;
    }
    const prototype = Object.getPrototypeOf(obj);

    return prototype === Object.prototype || prototype === null;
  };

  const setOwn = (target: Record<string, unknown>, key: string, value: unknown): void => {
    Object.defineProperty(target, key, {
      configurable: true,
      enumerable: true,
      value,
      writable: true,
    });
  };

  const cloneValue = (value: unknown): unknown => {
    if (Array.isArray(value)) {
      return value.map(cloneValue);
    }
    if (isMergeableObject(value)) {
      return merge({}, value);
    }

    return value;
  };

  // Prototype-polluting keys. A crafted source (e.g. parsed YAML/JSON with an
  // own "__proto__"/"constructor"/"prototype" key) would otherwise write
  // through to Object.prototype process-wide.
  const isUnsafeKey = (key: string): boolean => key === '__proto__' || key === 'constructor' || key === 'prototype';

  const merge = (target: Record<string, unknown>, source: Record<string, unknown> = {}): A & B => {
    const result: Record<string, unknown> = {};

    // Copy each base key exactly once, recursing (and thus cloning) only where
    // the source overrides it. Base branches the source leaves untouched are
    // deep-cloned so the result never shares nested references with the inputs.
    for (const key of Object.keys(target)) {
      const targetValue = target[key];

      if (Object.hasOwn(source, key) && !isUnsafeKey(key)) {
        const sourceValue = source[key];

        if (isMergeableObject(targetValue) && isMergeableObject(sourceValue)) {
          setOwn(result, key, merge(targetValue, sourceValue));
        } else {
          setOwn(result, key, cloneValue(sourceValue));
        }
      } else {
        setOwn(result, key, cloneValue(targetValue));
      }
    }

    // Append keys that only exist in the source.
    for (const key of Object.keys(source)) {
      if (isUnsafeKey(key) || Object.hasOwn(target, key)) {
        continue;
      }
      setOwn(result, key, cloneValue(source[key]));
    }

    return result as A & B;
  };

  // A nullish base is treated as an empty object.
  return merge((a ?? {}) as Record<string, unknown>, b as Record<string, unknown>);
}

/**
 * Sorts the keys of an object alphabetically, returning a new object
 * with the same key-value pairs in sorted order.
 *
 * @template T - The object type (must extend Record<string, unknown>)
 * @param obj - The object whose keys should be sorted
 * @returns A new object with alphabetically sorted keys, or `undefined` if input is falsy
 *
 * @example
 * ```typescript
 * sortObjectKeys({ z: 1, a: 2, m: 3 });
 * // { a: 2, m: 3, z: 1 }
 *
 * sortObjectKeys({ dependencies: { react: '^18.0.0', axios: '^1.0.0' } });
 * // { dependencies: { react: '^18.0.0', axios: '^1.0.0' } }
 * // Note: Only top-level keys are sorted
 * ```
 */
export function sortObjectKeys<T extends Record<string, unknown> = Record<string, unknown>>(obj?: T): T | undefined {
  if (!obj) {
    return undefined;
  }
  const sortedKeys = Object.keys(obj).sort();
  const sorted = sortedKeys.reduce((ac: T, key) => {
    (ac as Record<string, unknown>)[key] = obj[key];

    return ac;
  }, {} as T);

  return sorted;
}

/**
 * Determines whether a value is a plain object, i.e. one created via an object
 * literal or `new Object()` / `Object.create(null)`. Arrays, `Date`, `RegExp`,
 * `Map`, `Set`, functions, and class instances are not plain objects.
 */
function isPlainObjectValue(value: unknown): value is Record<string, unknown> {
  if (value === null || typeof value !== 'object') {
    return false;
  }
  const prototype = Object.getPrototypeOf(value);

  return prototype === Object.prototype || prototype === null;
}

/**
 * Creates a new object with all `undefined` values removed.
 *
 * Only plain objects are recursed into; every other value type — arrays, `Date`,
 * `RegExp`, `Map`, `Set`, functions, and class instances — is passed through
 * unchanged (same reference, preserving its prototype and internal state).
 *
 * @template T - The object type
 * @param obj - The object to process
 * @returns A new object without `undefined` values
 *
 * @example
 * ```typescript
 * omitUndefinedValues({ a: 1, b: undefined, c: { d: undefined, e: 2 } });
 * // { a: 1, c: { e: 2 } }
 *
 * omitUndefinedValues({ date: new Date(), value: null });
 * // { date: Date, value: null } - Dates and null are preserved
 *
 * omitUndefinedValues({ re: /x/, map: new Map(), fn: () => 42 });
 * // { re: /x/, map: Map, fn: [Function] } - non-plain objects pass through unchanged
 * ```
 */
export function omitUndefinedValues<T extends object>(obj: T): T {
  const result: Partial<T> = {};
  for (const [key, value] of Object.entries(obj)) {
    if (isPlainObjectValue(value)) {
      result[key as keyof T] = omitUndefinedValues(value) as T[keyof T];
    } else if (value !== undefined) {
      result[key as keyof T] = value as T[keyof T];
    }
  }

  return result as T;
}
