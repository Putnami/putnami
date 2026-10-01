import { StorageError } from '../errors';

/**
 * Validates an object key for path-traversal and null-byte injection.
 *
 * Backend-agnostic (no filesystem dependency) so both the file backend and the
 * remote/HTTP backend can enforce the same rules without the remote path
 * pulling in `node:fs`.
 */
export function validateKey(key: string): void {
  if (!key || key.includes('\0')) {
    throw new StorageError('Invalid storage key: empty or contains null bytes', 'INVALID_KEY');
  }
  // Reject any ".." path segment (start, middle, or end)
  const segments = key.split('/');
  for (const segment of segments) {
    if (segment === '..') {
      throw new StorageError(`Invalid storage key: path traversal detected in "${key}"`, 'PATH_TRAVERSAL');
    }
  }
  // Reject absolute paths
  if (key.startsWith('/')) {
    throw new StorageError(`Invalid storage key: absolute path not allowed "${key}"`, 'INVALID_KEY');
  }
}

/**
 * Percent-encodes each `/`-separated segment of a key or bucket so it cannot
 * inject query strings, fragments, extra path separators, or CRLF into a
 * request URL, while preserving the `/` separators that delimit virtual folders.
 */
export function encodeKeyPath(value: string): string {
  return value.split('/').map(encodeURIComponent).join('/');
}
