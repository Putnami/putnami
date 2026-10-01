/**
 * `content bump` core: the SOLE channel-resolution point of the content
 * pipeline. Resolves each lock entry's `package@channelHint` to the
 * channel's current blob, computes the sha256 digest FROM THE BYTES (headers
 * are provenance, never integrity), and rewrites `content.lock.json`. The
 * committed lock diff is the change detection — generate itself never asks a
 * registry what is new.
 *
 * Kept free of process/filesystem concerns so it is unit-testable; the
 * `scripts/content-bump.ts` CLI is a thin shell. A `putnami content bump` CLI
 * verb is deliberately deferred until a second site needs one.
 */
import type { ContentLock, LockBundle } from './lock';
import { sha256Hex } from './verify';

export interface ResolvedBundle {
  /** Version the registry advertised for the channel (provenance). */
  version: string;
  /** The bundle blob the channel currently points at. */
  blob: Uint8Array;
}

/** Injectable seam: how `package@channel` resolves — tests substitute doubles. */
export type ChannelResolver = (bundle: LockBundle) => Promise<ResolvedBundle>;

export interface BumpChange {
  name: string;
  fromDigest: string;
  toDigest: string;
  fromVersion: string;
  toVersion: string;
  /** The resolved blob, so the caller can prime the digest-keyed cache. */
  blob: Uint8Array;
}

export interface BumpResult {
  lock: ContentLock;
  changes: BumpChange[];
}

/**
 * Re-resolve every bundle (or the `only` subset) and return the updated lock.
 * Unchanged digests produce no change entry, so a no-op bump is visible.
 */
export async function bumpContentLock(
  lock: ContentLock,
  resolve: ChannelResolver,
  only?: string[],
): Promise<BumpResult> {
  const changes: BumpChange[] = [];
  const bundles: LockBundle[] = [];
  for (const bundle of lock.bundles) {
    if (only !== undefined && !only.includes(bundle.name)) {
      bundles.push(bundle);
      continue;
    }
    const resolved = await resolve(bundle);
    // The lock requires a non-empty version (parseContentLock rejects ""), so
    // never let a resolver write an unparseable lock — fail the bump instead.
    if (resolved.version.trim() === '') {
      throw new Error(
        `content bump: resolver returned an empty version for bundle ${JSON.stringify(bundle.name)} ` +
          `(${bundle.package}@${bundle.channelHint}); the lock requires a version, refusing to write one that will not parse`,
      );
    }
    const digest = sha256Hex(resolved.blob);
    if (digest === bundle.digest && resolved.version === bundle.version) {
      bundles.push(bundle);
      continue;
    }
    changes.push({
      name: bundle.name,
      fromDigest: bundle.digest,
      toDigest: digest,
      fromVersion: bundle.version,
      toVersion: resolved.version,
      blob: resolved.blob,
    });
    bundles.push({ ...bundle, version: resolved.version, digest });
  }
  return { lock: { bundles }, changes };
}
