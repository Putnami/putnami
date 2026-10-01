/**
 * Shared Utilities - Browser and SSR Compatible
 *
 * This module exports all utilities that work in both browser and server environments.
 * These utilities have no dependencies on Node.js-specific modules.
 *
 * @module @putnami/utils
 */

// Array manipulation utilities
export { chunk, ensureArray, first, last, unique } from './array.utils';
// HTML escaping utilities
export { escapeHtml } from './html.utils';
// Object manipulation utilities
export {
  getDeep,
  mergeDeep,
  omitUndefinedValues,
  sortObjectKeys,
} from './object.utils';
// Task processing utilities
export { TaskQueue } from './task.utils';
// Type checking utilities
export { isFunction, isNullish, isObject, isPlainObject, isString, type Promisable, throwError } from './type.utils';
// URL utilities
export { buildUrlWithParams, getBaseUrl, joinUrlPath, parseQueryString } from './url.utils';
// Schema validation utilities
export * from './schema';
