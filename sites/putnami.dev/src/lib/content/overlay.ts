/**
 * Runtime overlay manager for site-content bundles.
 *
 * Holds the single-process ACTIVE pointer the docs-content readers resolve
 * through: `null` means "serve baked content" (the always-available fallback),
 * a path means "serve this fully-materialized overlay version". Each running
 * instance converges independently — acceptable per the issue (putnami.dev
 * deploys with max one instance today) and it keeps the design boring: no
 * shared state, no cross-instance coordination.
 *
 * Atomicity model:
 * - ON DISK, a version is materialized entirely into a `.staging-<digest>`
 *   sibling, marked complete, and then `rename(2)`d to `versions/<digest>`.
 *   The MARKER is the completeness proof, not the directory: these live under
 *   the OS temp dir, whose reaper deletes files past its retention and leaves
 *   the directory tree standing, so a bare directory proves nothing.
 * - IN MEMORY, {@link setActive} is the swap: it is only called AFTER the
 *   on-disk finalize, so a reader either resolves the previous root or the new
 *   one, never a half-materialized directory. A version emptied AFTER it was
 *   activated is caught on the read side — see {@link deactivateOverlay}.
 */
import { readdirSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';

let activeDocsRoot: string | null = null;
let activeDigest: string | null = null;

/**
 * Machine-local directory holding finalized overlay versions, one directory
 * per bundle digest. Lives under the OS temp dir: overlay state is a cache of
 * registry content, losing it on restart just means serving baked content
 * until the next newness check converges again.
 *
 * That reasoning covers a TOTAL loss. It does not cover the PARTIAL loss a
 * temp reaper produces — files gone, directories left — which is why
 * completeness is proven by a marker and re-checked on the read side.
 */
export function overlayVersionsDir(baseDir: string = tmpdir()): string {
  return join(baseDir, 'putnami-site-content-overlay', 'versions');
}

/** The docs root readers must serve from: the overlay when active, else baked. */
export function getActiveDocsRoot(bakedRoot: string): string {
  return activeDocsRoot ?? bakedRoot;
}

/** Digest of the active overlay bundle, or null when baked content serves. */
export function activeContentDigest(): string | null {
  return activeDigest;
}

/**
 * Flip the in-memory pointer to a finalized version's docs root. Callers must
 * only pass directories that have been fully materialized and renamed to their
 * final `versions/<digest>` path (see the atomicity model above). Older
 * version directories are then GC'd best-effort — the current version is kept
 * and baked content is never touched.
 */
export function setActive(versionDocsRoot: string, digest: string): void {
  activeDocsRoot = versionDocsRoot;
  activeDigest = digest;
  // versionDocsRoot = <versionsDir>/<digest>/docs → GC siblings of <digest>.
  gcVersions(dirname(dirname(versionDocsRoot)), digest);
}

/**
 * Stand down a damaged overlay: drop the active pointer so readers resolve
 * baked content again, and return the digest that was active (null when baked
 * was already serving, so a caller can tell a real deactivation from a no-op).
 *
 * The atomicity model above guarantees a version directory is COMPLETE when it
 * is activated. It cannot guarantee it stays complete: the versions live under
 * the OS temp dir, and a temp reaper that deletes files while leaving the
 * directory tree standing empties an already-active version out from under the
 * readers. Because the overlay REPLACES the baked root rather than
 * layering over it, an emptied version 404s every docs page while a complete
 * baked tree sits unread — so a reader that observes an empty overlay calls
 * this and serves baked instead.
 *
 * Recovery is automatic: with no active digest the next newness check compares
 * the channel head against the lock pin and re-ingests, which re-materializes
 * the version because its completeness marker is gone too.
 */
export function deactivateOverlay(): string | null {
  const wasActive = activeDigest;
  activeDocsRoot = null;
  activeDigest = null;
  return wasActive;
}

/** Test helper: reset the module-scope pointer (baked content serves again). */
export function resetOverlayForTest(): void {
  activeDocsRoot = null;
  activeDigest = null;
}

/** Best-effort removal of every non-active version (and stale staging dirs). */
function gcVersions(versionsDir: string, keepDigest: string): void {
  let entries: string[];
  try {
    entries = readdirSync(versionsDir);
  } catch {
    // Nothing to GC (first activation, or a caller-provided dir already gone).
    return;
  }
  for (const entry of entries) {
    if (entry === keepDigest) continue;
    try {
      rmSync(join(versionsDir, entry), { recursive: true, force: true });
    } catch {
      // best-effort cleanup — a busy file just leaves a stale dir for next GC
    }
  }
}
