/**
 * Type Checking Utility Functions
 *
 * Browser and SSR compatible type checking utilities.
 * @module @putnami/utils
 */

/**
 * Checks if a value is a string (primitive or String object).
 *
 * @param value - The value to check
 * @returns `true` if the value is a string, `false` otherwise
 *
 * @example
 * ```typescript
 * isString('hello');          // true
 * isString(new String('hi')); // true
 * isString(123);              // false
 * isString(null);             // false
 * ```
 */
export function isString(value?: unknown): value is string {
  return typeof value === 'string' || value instanceof String;
}

/**
 * Checks if a value is an object (including functions, excluding null).
 *
 * @param value - The value to check
 * @returns `true` if the value is an object or function, `false` otherwise
 *
 * @example
 * ```typescript
 * isObject({});              // true
 * isObject([]);              // true
 * isObject(() => {});        // true
 * isObject(null);            // false
 * isObject(undefined);       // false
 * isObject('string');        // false
 * ```
 */
export function isObject(value: unknown): value is object {
  return typeof value === 'object' ? value !== null : typeof value === 'function';
}

/**
 * Checks if a value is a plain object (not an array, date, or other special object).
 *
 * @param value - The value to check
 * @returns `true` if the value is a plain object, `false` otherwise
 *
 * @example
 * ```typescript
 * isPlainObject({});           // true
 * isPlainObject({ a: 1 });     // true
 * isPlainObject([]);           // false
 * isPlainObject(new Date());   // false
 * isPlainObject(null);         // false
 * ```
 */
export function isPlainObject(value: unknown): value is Record<string, unknown> {
  return !!value && typeof value === 'object' && !Array.isArray(value) && !(value instanceof Date);
}

/**
 * Checks if a value is null or undefined.
 *
 * @param value - The value to check
 * @returns `true` if the value is null or undefined, `false` otherwise
 *
 * @example
 * ```typescript
 * isNullish(null);        // true
 * isNullish(undefined);   // true
 * isNullish(0);           // false
 * isNullish('');          // false
 * isNullish(false);       // false
 * ```
 */
export function isNullish(value: unknown): value is null | undefined {
  return value === null || value === undefined;
}

/**
 * Checks if a value is a function.
 *
 * @param value - The value to check
 * @returns `true` if the value is a function, `false` otherwise
 *
 * @example
 * ```typescript
 * isFunction(() => {});           // true
 * isFunction(function() {});      // true
 * isFunction(class {});           // true
 * isFunction({});                 // false
 * ```
 */
// biome-ignore lint/complexity/noBannedTypes: Function type is intentional here
export function isFunction(value: unknown): value is Function {
  return typeof value === 'function';
}

/**
 * Throws an error with the given message. This function never returns.
 *
 * Useful for expressing unreachable code paths or inline error throwing.
 *
 * @param message - The error message
 * @throws Always throws an Error with the given message
 * @returns Never returns (always throws)
 *
 * @example
 * ```typescript
 * // Inline error throwing in expressions
 * const value = maybeValue ?? throwError('Value is required');
 *
 * // Exhaustive switch checking
 * switch (status) {
 *   case 'pending': return handlePending();
 *   case 'done': return handleDone();
 *   default: throwError(`Unknown status: ${status}`);
 * }
 * ```
 */
export function throwError(message: string): never {
  throw new Error(message);
}

export type Promisable<T> = T | Promise<T>;
