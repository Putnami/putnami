/**
 * Array Utility Functions
 *
 * Browser and SSR compatible array manipulation utilities.
 * @module @putnami/utils
 */

/**
 * Ensures a value is wrapped in an array. If the value is already an array,
 * it is returned as-is. Otherwise, it is wrapped in an array.
 *
 * @template T - The type of the value(s)
 * @param value - The value or array to process
 * @returns An array containing the value(s)
 *
 * @example
 * ```typescript
 * ensureArray('single');        // ['single']
 * ensureArray(['a', 'b']);      // ['a', 'b']
 * ensureArray(undefined);       // [undefined]
 * ensureArray([]);              // []
 * ```
 */
export function ensureArray<T>(value: T | T[]): T[] {
  return Array.isArray(value) ? value : [value];
}

/**
 * Returns the first element of an array, or undefined if the array is empty.
 *
 * @template T - The type of array elements
 * @param arr - The array to get the first element from
 * @returns The first element or undefined
 *
 * @example
 * ```typescript
 * first([1, 2, 3]);     // 1
 * first([]);            // undefined
 * first(['a', 'b']);    // 'a'
 * ```
 */
export function first<T>(arr: T[]): T | undefined {
  return arr[0];
}

/**
 * Returns the last element of an array, or undefined if the array is empty.
 *
 * @template T - The type of array elements
 * @param arr - The array to get the last element from
 * @returns The last element or undefined
 *
 * @example
 * ```typescript
 * last([1, 2, 3]);     // 3
 * last([]);            // undefined
 * last(['a', 'b']);    // 'b'
 * ```
 */
export function last<T>(arr: T[]): T | undefined {
  return arr[arr.length - 1];
}

/**
 * Removes duplicate values from an array.
 *
 * @template T - The type of array elements
 * @param arr - The array to deduplicate
 * @returns A new array with unique values
 *
 * @example
 * ```typescript
 * unique([1, 2, 2, 3, 1]);     // [1, 2, 3]
 * unique(['a', 'b', 'a']);     // ['a', 'b']
 * ```
 */
export function unique<T>(arr: T[]): T[] {
  return [...new Set(arr)];
}

/**
 * Chunks an array into smaller arrays of a specified size.
 *
 * @template T - The type of array elements
 * @param arr - The array to chunk
 * @param size - The size of each chunk
 * @returns An array of chunks
 *
 * @example
 * ```typescript
 * chunk([1, 2, 3, 4, 5], 2);   // [[1, 2], [3, 4], [5]]
 * chunk([1, 2, 3], 3);         // [[1, 2, 3]]
 * chunk([], 2);                // []
 * ```
 */
export function chunk<T>(arr: T[], size: number): T[][] {
  if (!Number.isInteger(size) || size < 1) {
    throw new Error(`chunk: size must be a positive integer, received ${size}`);
  }
  const chunks: T[][] = [];
  for (let i = 0; i < arr.length; i += size) {
    chunks.push(arr.slice(i, i + size));
  }
  return chunks;
}
