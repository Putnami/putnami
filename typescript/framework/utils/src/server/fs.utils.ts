/**
 * File System Utility Functions (Server Only)
 *
 * These utilities wrap Node.js fs functions and provide Bun-optimized
 * alternatives where available. Only available in server-side environments.
 *
 * @module @putnami/utils
 */

import {
  copyFileSync as copyFileSyncNode,
  cpSync as cpSyncNode,
  existsSync as existsSyncNode,
  mkdirSync as mkdirSyncNode,
  mkdtempSync as mkdtempSyncNode,
  readdirSync as readdirSyncNode,
  readFileSync as readFileSyncNode,
  rmSync as rmSyncNode,
  statSync as statSyncNode,
  unlinkSync as unlinkSyncNode,
  watch as watchNode,
  writeFileSync as writeFileSyncNode,
} from 'node:fs';
import { mkdir, readdir, rm, stat } from 'node:fs/promises';

// ============================================================================
// Synchronous File Operations (for CLI tooling)
// ============================================================================

/**
 * Checks if a file or directory exists at the given path.
 *
 * @param path - The path to check
 * @returns `true` if the path exists
 *
 * @example
 * ```typescript
 * if (fileExists('/path/to/file.txt')) {
 *   // File exists
 * }
 * ```
 */
export const fileExists = existsSyncNode;

/**
 * Reads the entire contents of a file synchronously.
 *
 * @param path - The path to the file
 * @param encoding - Optional encoding (defaults to returning a Buffer)
 * @returns The file contents as a string or Buffer
 *
 * @example
 * ```typescript
 * const content = readFileContent('/path/to/file.txt', 'utf-8');
 * ```
 */
export const readFileContent = readFileSyncNode;

/**
 * Writes data to a file synchronously, replacing the file if it exists.
 *
 * @param path - The path to the file
 * @param data - The data to write
 * @param options - Optional encoding or write options
 *
 * @example
 * ```typescript
 * writeFileContent('/path/to/file.txt', 'Hello, World!', 'utf-8');
 * ```
 */
export const writeFileContent = writeFileSyncNode;

/**
 * Creates a directory synchronously. If the parent directories don't exist,
 * they are created when `recursive: true` is passed.
 *
 * @param path - The directory path to create
 * @param options - Optional options including `recursive` flag
 *
 * @example
 * ```typescript
 * makeDir('/path/to/new/dir', { recursive: true });
 * ```
 */
export const makeDir = mkdirSyncNode;

/**
 * Removes a file or directory synchronously.
 *
 * @param path - The path to remove
 * @param options - Optional options including `recursive` and `force` flags
 *
 * @example
 * ```typescript
 * removeDir('/path/to/dir', { recursive: true, force: true });
 * ```
 */
export const removeDir = rmSyncNode;

/**
 * Reads the contents of a directory synchronously.
 *
 * @param path - The directory path to read
 * @param options - Optional options including `withFileTypes` flag
 * @returns Array of filenames or directory entries
 *
 * @example
 * ```typescript
 * const files = listDir('/path/to/dir');
 * const entries = listDir('/path/to/dir', { withFileTypes: true });
 * ```
 */
export const listDir = readdirSyncNode;

/**
 * Gets file or directory statistics synchronously.
 *
 * @param path - The path to stat
 * @returns The file stats object
 *
 * @example
 * ```typescript
 * const stats = fileStat('/path/to/file.txt');
 * if (stats.isDirectory()) {
 *   // It's a directory
 * }
 * ```
 */
export const fileStat = statSyncNode;

/**
 * Copies a file synchronously.
 *
 * @param src - The source file path
 * @param dest - The destination file path
 *
 * @example
 * ```typescript
 * copyFile('/path/to/source.txt', '/path/to/dest.txt');
 * ```
 */
export const copyFile = copyFileSyncNode;

/**
 * Copies a file or directory recursively synchronously.
 *
 * @param src - The source path
 * @param dest - The destination path
 * @param options - Optional options including `recursive` flag
 *
 * @example
 * ```typescript
 * copyDir('/path/to/source', '/path/to/dest', { recursive: true });
 * ```
 */
export const copyDir = cpSyncNode;

/**
 * Deletes a file synchronously.
 *
 * @param path - The file path to delete
 *
 * @example
 * ```typescript
 * deleteFile('/path/to/file.txt');
 * ```
 */
export const deleteFile = unlinkSyncNode;

/**
 * Creates a unique temporary directory synchronously.
 *
 * @param prefix - The prefix for the temporary directory name
 * @returns The path to the created temporary directory
 *
 * @example
 * ```typescript
 * const tempDir = makeTempDir('/tmp/myapp-');
 * // Returns something like '/tmp/myapp-abc123'
 * ```
 */
export const makeTempDir = mkdtempSyncNode;

/**
 * Watches for changes on a file or directory.
 *
 * @param path - The path to watch
 * @param options - Optional watch options
 * @param listener - Callback function for change events
 * @returns A FSWatcher instance
 *
 * @example
 * ```typescript
 * const watcher = watchPath('/path/to/dir', (eventType, filename) => {
 *   console.log(`${filename} was ${eventType}`);
 * });
 * ```
 */
export const watchPath = watchNode;

// ============================================================================
// Async File Operations (for runtime/server code)
// ============================================================================

/**
 * Reads a file asynchronously using Bun's optimized file API.
 *
 * @param path - The path to the file
 * @returns Promise resolving to the file contents as a string
 *
 * @example
 * ```typescript
 * const content = await readFileAsync('/path/to/file.txt');
 * ```
 */
export async function readFileAsync(path: string): Promise<string> {
  return Bun.file(path).text();
}

/**
 * Reads a file as bytes asynchronously using Bun's optimized file API.
 *
 * @param path - The path to the file
 * @returns Promise resolving to the file contents as Uint8Array
 *
 * @example
 * ```typescript
 * const bytes = await readFileBytesAsync('/path/to/file.bin');
 * ```
 */
export async function readFileBytesAsync(path: string): Promise<Uint8Array> {
  return Bun.file(path).bytes();
}

/**
 * Writes data to a file asynchronously using Bun's optimized file API.
 *
 * @param path - The path to the file
 * @param data - The data to write
 * @returns Promise resolving to the number of bytes written
 *
 * @example
 * ```typescript
 * await writeFileAsync('/path/to/file.txt', 'Hello, World!');
 * ```
 */
export async function writeFileAsync(path: string, data: string | Uint8Array | ArrayBuffer | Blob): Promise<number> {
  return Bun.write(path, data);
}

/**
 * Checks if a file exists asynchronously using Bun's file API.
 *
 * @param path - The path to check
 * @returns Promise resolving to `true` if the file exists
 *
 * @example
 * ```typescript
 * if (await fileExistsAsync('/path/to/file.txt')) {
 *   // File exists
 * }
 * ```
 */
export async function fileExistsAsync(path: string): Promise<boolean> {
  return Bun.file(path).exists();
}

/**
 * Gets the size of a file asynchronously using Bun's file API.
 *
 * @param path - The path to the file
 * @returns Promise resolving to the file size in bytes
 *
 * @example
 * ```typescript
 * const size = await fileSizeAsync('/path/to/file.txt');
 * ```
 */
export async function fileSizeAsync(path: string): Promise<number> {
  return Bun.file(path).size;
}

// Re-export promise-based Node.js fs functions for operations without Bun equivalents
export {
  /** Creates a directory asynchronously */
  mkdir as makeDirAsync,
  /** Reads directory contents asynchronously */
  readdir as listDirAsync,
  /** Removes files/directories asynchronously */
  rm as removeDirAsync,
  /** Gets file stats asynchronously */
  stat as fileStatAsync,
};
