/**
 * Path Utility Functions (Server Only)
 *
 * These utilities wrap Node.js path functions and are only available
 * in server-side environments (Bun, Node.js).
 *
 * @module @putnami/utils
 */

import {
  basename as basenameNode,
  dirname as dirnameNode,
  extname as extnameNode,
  isAbsolute as isAbsoluteNode,
  join as joinNode,
  normalize as normalizeNode,
  posix,
  relative as relativeNode,
  resolve as resolveNode,
} from 'node:path';

/**
 * Joins path segments using the platform-specific separator.
 *
 * @param paths - Path segments to join
 * @returns The joined path
 *
 * @example
 * ```typescript
 * joinPath('folder', 'subfolder', 'file.txt');
 * // 'folder/subfolder/file.txt' (on Unix)
 * // 'folder\\subfolder\\file.txt' (on Windows)
 * ```
 */
export const joinPath = joinNode;

/**
 * Converts every backslash in a path to a forward slash.
 *
 * Use it for a path written into generated source text: an import specifier,
 * an identifier derived from a path, a route key or a manifest path. On
 * Windows the native separator is a backslash, and inside a string literal it
 * starts an escape sequence (`'..\not-found'` reads `\n` as a newline).
 * Filesystem calls keep native paths.
 *
 * @param path - A path in native or forward-slash form
 * @returns The same path with forward slashes
 *
 * @example
 * ```typescript
 * toPosixPath('..\\..\\src\\api\\get');
 * // '../../src/api/get'
 * ```
 */
export const toPosixPath = (path: string): string => path.replaceAll('\\', '/');

/**
 * Joins path segments with forward slashes on every platform.
 *
 * Segments in native Windows form are converted first, so the result is the
 * same whichever platform computed them. Use it to build generated source
 * text; use {@link joinPath} for filesystem paths.
 *
 * @param paths - Path segments to join
 * @returns The joined path in forward-slash form
 *
 * @example
 * ```typescript
 * joinPosixPath('..\\..\\src\\app', 'not-found');
 * // '../../src/app/not-found' (on every platform)
 * ```
 */
export const joinPosixPath = (...paths: string[]): string => posix.join(...paths.map(toPosixPath));

/**
 * Computes the relative path from one path to another.
 *
 * @param from - The starting path
 * @param to - The destination path
 * @returns The relative path from `from` to `to`
 *
 * @example
 * ```typescript
 * relativePath('/path/to/dir', '/path/to/dir/file.txt');
 * // 'file.txt'
 *
 * relativePath('/path/to/dir', '/path/other');
 * // '../other'
 * ```
 */
export const relativePath = relativeNode;

/**
 * Returns the directory name of a path.
 *
 * @param path - The file path
 * @returns The directory containing the path
 *
 * @example
 * ```typescript
 * getDirectoryName('/path/to/file.txt');
 * // '/path/to'
 *
 * getDirectoryName('file.txt');
 * // '.'
 * ```
 */
export const getDirectoryName = dirnameNode;

/**
 * Resolves a sequence of paths or path segments into an absolute path.
 *
 * @param paths - Path segments to resolve
 * @returns The resolved absolute path
 *
 * @example
 * ```typescript
 * resolvePath('/path', 'to', 'file.txt');
 * // '/path/to/file.txt'
 *
 * resolvePath('relative', 'path');
 * // '/current/working/dir/relative/path'
 * ```
 */
export const resolvePath = resolveNode;

/**
 * Returns the last portion of a path (the filename).
 *
 * @param path - The file path
 * @param suffix - Optional suffix to remove from the result
 * @returns The filename portion of the path
 *
 * @example
 * ```typescript
 * getBaseName('/path/to/file.txt');
 * // 'file.txt'
 *
 * getBaseName('/path/to/file.txt', '.txt');
 * // 'file'
 * ```
 */
export const getBaseName = basenameNode;

/**
 * Returns the extension of a path.
 *
 * @param path - The file path
 * @returns The extension including the dot, or empty string if no extension
 *
 * @example
 * ```typescript
 * getExtension('/path/to/file.txt');
 * // '.txt'
 *
 * getExtension('/path/to/file');
 * // ''
 * ```
 */
export const getExtension = extnameNode;

/**
 * Normalizes a path, resolving '..' and '.' segments.
 *
 * @param path - The path to normalize
 * @returns The normalized path
 *
 * @example
 * ```typescript
 * normalizePath('/path/to/../file.txt');
 * // '/path/file.txt'
 * ```
 */
export const normalizePath = normalizeNode;

/**
 * Determines whether a path is absolute.
 *
 * @param path - The path to check
 * @returns `true` if the path is absolute
 *
 * @example
 * ```typescript
 * isAbsolutePath('/path/to/file');
 * // true
 *
 * isAbsolutePath('relative/path');
 * // false
 * ```
 */
export const isAbsolutePath = isAbsoluteNode;
