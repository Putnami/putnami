/**
 * Newness check for the runtime content overlay: a TTL-gated,
 * single-flight, stale-while-revalidate poll of the bundle's cheap channel
 * POINTER (never the blob). Requests always serve the current content; the
 * check runs beside them, fire-and-forget, and converges the overlay when the
 * channel head moved. Cold start (no overlay) serves baked content and
 * converges on the first check.
 *
 * The bundle identity (package/registry/name/channel) comes from the
 * committed `content.lock.json`; the committed digest is the baked/fallback
 * pin used as the "current" digest until an overlay is active. v1 scope: the
 * site self-updates its single content bundle (the first lock entry).
 */
import { type LockBundle, readContentLock } from './lock';
import { type BlobFetch, type IngestOutcome, ingestBundle } from './ingest';
import { activeContentDigest } from './overlay';
import { type ChannelPointer, fetchChannelPointerFromRegistry } from './registry';

export interface ConvergeOptions {
  /** Path to the committed `content.lock.json`. */
  lockPath: string;
  /** Baked docs root (the always-available fallback the overlay unions). */
  bakedDocsRoot: string;
  /** URL prefixes owned by the baked bundle (pruned on ingest — see IngestOptions). */
  bakedMounts?: string[];
  /** Minimum interval between channel-pointer polls. */
  checkTtlMs: number;
  /** Max accepted blob size handed to the ingest pipeline. */
  sizeCapBytes: number;
  /** Version-directory location (ingest default: OS temp dir). */
  versionsDir?: string;
  /** Channel-pointer fetcher; defaults to the put-registry pointer client. */
  fetchPointer?: (bundle: LockBundle) => Promise<ChannelPointer>;
  /** Blob fetcher forwarded to the ingest pipeline. */
  fetchBlob?: BlobFetch;
  /** Outcome/warning sink; defaults to a stderr line writer. */
  log?: (message: string) => void;
  /** Post-swap invalidation forwarded to the ingest pipeline. */
  onSwap?: () => Promise<void>;
  /** Clock seam for the TTL gate (tests). */
  now?: () => number;
}

// Module-scope check state: one TTL window and at most one in-flight check per
// process (single-flight) — concurrent requests trigger at most one poll.
let lastCheckAt = 0;
let inFlight: Promise<IngestOutcome | undefined> | null = null;

/** Test helper: reset the TTL window and the single-flight guard. */
export function resetConvergeStateForTest(): void {
  lastCheckAt = 0;
  inFlight = null;
}

function defaultLog(message: string): void {
  process.stderr.write(`${message}\n`);
}

/**
 * TTL-gated, single-flight newness check. Returns the in-flight promise when
 * a check is running or was just started (tests and the refresh path await
 * it) and `undefined` when the TTL gate skipped. Serving-path callers must
 * NOT await the result — the check must never block a request.
 */
export function maybeCheckAndConverge(options: ConvergeOptions): Promise<IngestOutcome | undefined> | undefined {
  if (inFlight) return inFlight;
  const now = (options.now ?? Date.now)();
  if (lastCheckAt !== 0 && now - lastCheckAt < options.checkTtlMs) return undefined;
  return startCheck(options, now);
}

/**
 * Refresh-endpoint path: bypass the TTL but share the single-flight guard so
 * a refresh never overlaps a running poll (waits for it, then runs its own
 * check against the fresh channel head).
 */
export async function forceCheckAndConverge(options: ConvergeOptions): Promise<IngestOutcome | undefined> {
  if (inFlight) {
    await inFlight.catch(() => undefined);
  }
  return startCheck(options, (options.now ?? Date.now)());
}

function startCheck(options: ConvergeOptions, now: number): Promise<IngestOutcome | undefined> {
  const log = options.log ?? defaultLog;
  lastCheckAt = now;
  inFlight = checkOnce(options, log)
    .catch((error) => {
      // The overlay must never break serving: an unexpected check failure is
      // logged and the current content (overlay or baked) keeps serving.
      log(`content overlay: newness check failed: ${(error as Error).message}`);
      return undefined;
    })
    .finally(() => {
      inFlight = null;
    });
  return inFlight;
}

async function checkOnce(options: ConvergeOptions, log: (message: string) => void): Promise<IngestOutcome | undefined> {
  const lock = readContentLock(options.lockPath);
  if (lock.bundles.length === 0) {
    // Empty lock: the site pins no bundle, there is nothing to converge to.
    return undefined;
  }
  const bundle = lock.bundles[0];

  const fetchPointer =
    options.fetchPointer ?? ((b: LockBundle) => fetchChannelPointerFromRegistry(b.package, b.channelHint, b.registry));
  let pointer: ChannelPointer;
  try {
    pointer = await fetchPointer(bundle);
  } catch (error) {
    const reason = `channel pointer fetch failed: ${(error as Error).message}`;
    log(`content overlay: ${bundle.package}@${bundle.channelHint}: ${reason} — keeping current content`);
    return { outcome: 'fetch-failed', activeDigest: activeContentDigest(), reason };
  }

  // "Current" = the active overlay digest, or the baked lock pin before any
  // overlay converged (cold start).
  const current = activeContentDigest() ?? bundle.digest;
  if (pointer.digest === current) {
    return { outcome: 'unchanged', activeDigest: activeContentDigest() };
  }

  return ingestBundle({
    bundle,
    digest: pointer.digest,
    bakedDocsRoot: options.bakedDocsRoot,
    bakedMounts: options.bakedMounts,
    sizeCapBytes: options.sizeCapBytes,
    versionsDir: options.versionsDir,
    fetchBlob: options.fetchBlob,
    log,
    onSwap: options.onSwap,
  });
}
