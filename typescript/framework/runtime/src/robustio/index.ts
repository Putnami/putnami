/**
 * Renames and removals that wait out Windows file sharing. On Windows a rename
 * or a removal fails for a moment while another process, such as a reader of
 * the file, an antivirus scanner or the search indexer, holds the file without
 * sharing delete access. These functions retry that failure for at most two
 * seconds, as `go.putnami.dev/sdk/extension/robustio` does in Go. On every
 * other platform they call the `node:fs` function once.
 */
import { renameSync, rmSync } from 'node:fs';
import { rename, rm } from 'node:fs/promises';
import { retry, retrySync } from './retry';

/** `rename` from `node:fs/promises`, retried on Windows while another process holds the file. */
export function robustRename(oldPath: string, newPath: string): Promise<void> {
  return retry(() => rename(oldPath, newPath));
}

/** `renameSync` from `node:fs`, retried on Windows while another process holds the file. */
export function robustRenameSync(oldPath: string, newPath: string): void {
  retrySync(() => renameSync(oldPath, newPath));
}

/**
 * Removes the file at `path`; a missing file is not an error, as with `rm` and
 * `force`. Retried on Windows while another process holds the file.
 */
export function robustRemove(path: string): Promise<void> {
  return retry(() => rm(path, { force: true }));
}

/** The synchronous form of {@link robustRemove}. */
export function robustRemoveSync(path: string): void {
  retrySync(() => rmSync(path, { force: true }));
}
