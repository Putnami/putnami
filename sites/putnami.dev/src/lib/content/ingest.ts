/**
 * Runtime ingest pipeline for site-content bundles.
 *
 * Converges the RUNNING site to a newer published bundle version without a
 * CI/CD cycle: fetch by digest → verify (the exact contract checks the baked
 * pipeline enforces, reused from `verify.ts`) → materialize a complete docs
 * tree (baked content + the bundle's mounts, bundle wins per mount) into a
 * per-digest version directory → rebuild the search index → finalize on disk
 * with an atomic rename → flip the in-memory active pointer.
 *
 * Trust model: unlike generate-time materialization (pinned by the committed
 * lock digest), the runtime target digest comes from the registry's channel
 * pointer — self-update inherently trusts the registry for content. The
 * digest still authenticates the fetched bytes against the pointer, and every
 * structural payload rule (paths, mounts, per-file digests, format major)
 * is enforced before anything is served.
 *
 * Failure policy: NEVER throw toward the serving path. Every failure mode
 * maps to a typed {@link IngestOutcome} and the currently active content
 * (overlay or baked) keeps serving. Markdown is NOT pre-rendered here — the
 * docs loader's `renderMarkdown()` fallback renders overlay pages on demand.
 */
import { cpSync, mkdirSync, renameSync, rmSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { revalidateTag } from '@putnami/web';
import { fileExists } from '@putnami/utils';
import { invalidateNavCache } from '../docs/navigation.server';
import { buildSearchIndex } from '../search/build-index';
import type { LockBundle } from './lock';
import { activeContentDigest, overlayVersionsDir, setActive } from './overlay';
import { fetchBundleBlobFromRegistry } from './registry';
import { type Diagnostic, validDigest } from './sitecontent';
import { sha256Hex, type VerifiedBundle, verifyBundleArchive } from './verify';

/** Injectable seam: how a bundle blob is fetched (tests substitute doubles). */
export type BlobFetch = (bundle: Pick<LockBundle, 'package' | 'digest' | 'registry'>) => Promise<Uint8Array>;

export type IngestOutcomeKind =
  | 'updated'
  | 'unchanged'
  | 'rejected-digest'
  | 'rejected-format'
  | 'rejected-size'
  | 'fetch-failed';

export interface IngestOutcome {
  outcome: IngestOutcomeKind;
  /** Digest of the content active AFTER this ingest (null = baked). */
  activeDigest: string | null;
  /** Human-readable cause for non-`updated` outcomes (observability). */
  reason?: string;
}

export interface IngestOptions {
  /** Bundle identity from the committed lock (name/package/registry). */
  bundle: Pick<LockBundle, 'name' | 'package' | 'registry'>;
  /** Target blob digest (from the channel pointer or an explicit refresh). */
  digest: string;
  /** Baked docs root the overlay version unions bundle mounts into. */
  bakedDocsRoot: string;
  /**
   * URL prefixes owned by the bundle BAKED into the image (from the
   * generate-time `.gen/content-bundles.manifest.json` sidecar). They are
   * cleared from the copied baked tree before the new version is written, so a
   * channel version that DROPS or RENAMES a mount leaves no stale baked docs in
   * the overlay. Empty when no bundle is baked (an empty lock).
   */
  bakedMounts?: string[];
  /** Max accepted blob size — tmpfs/memory footprint guard. */
  sizeCapBytes: number;
  /** Version-directory location; defaults to {@link overlayVersionsDir}. */
  versionsDir?: string;
  /** Blob fetcher; defaults to the put-registry fetch-by-digest client. */
  fetchBlob?: BlobFetch;
  /** Outcome/warning sink; defaults to a stderr line writer. */
  log?: (message: string) => void;
  /** Post-swap invalidation; defaults to nav-cache reset + `revalidateTag('docs')`. */
  onSwap?: () => Promise<void>;
}

/**
 * Marker written LAST into a staging tree, so it can only reach a version's
 * final path attached to a complete materialization. Its presence — not the
 * directory's — is what proves a version is servable.
 */
const VERSION_COMPLETE_MARKER = '.complete';

/**
 * Whether `versionDir` holds a COMPLETE materialization.
 *
 * The directory's own existence is not proof: versions live under the
 * OS temp dir, and a temp reaper deletes files older than its retention while
 * leaving the directory tree standing. A reaped version is an empty skeleton
 * that would otherwise pass an existence check, get activated, and 404 every
 * docs page. Reaping the marker along with the content is exactly what makes
 * this check fail closed — the version re-materializes instead.
 */
function isVersionComplete(versionDir: string): boolean {
  return fileExists(join(versionDir, VERSION_COMPLETE_MARKER));
}

function formatDiagnostics(diags: Diagnostic[]): string {
  return diags.map((d) => `${d.code}${d.field ? ` at ${d.field}` : ''}: ${d.message}`).join('; ');
}

function defaultLog(message: string): void {
  process.stderr.write(`${message}\n`);
}

/** Default post-swap invalidation: nav caches + ISR pages tagged 'docs'. */
async function defaultOnSwap(): Promise<void> {
  invalidateNavCache();
  await revalidateTag('docs');
}

function isDocsPrefix(urlPrefix: string): boolean {
  return urlPrefix === '/docs' || urlPrefix.startsWith('/docs/');
}

/**
 * Materialize a complete docs tree for one bundle version into `staging`:
 * copy the baked tree, clear the ENTIRE bundle-owned region (the baked bundle's
 * mounts plus the new manifest's mounts), then write the bundle files in
 * deterministic (path-sorted) order. Clearing the baked mounts too — not only
 * the ones the new version still declares — is what prevents a DROPPED or
 * RENAMED mount from leaving stale baked docs behind (mirrors the generate-time
 * cross-run cleanup in materialize.ts). Mounts outside `/docs` cannot be
 * overlay-served (the staticFiles plugin serves them from baked output only), so
 * their files are skipped with a warning and keep serving from baked content.
 */
function materializeVersion(
  staging: string,
  bakedDocsRoot: string,
  bakedMounts: string[],
  verified: VerifiedBundle,
  log: (message: string) => void,
): void {
  const stagingDocs = join(staging, 'docs');
  mkdirSync(stagingDocs, { recursive: true });
  if (fileExists(bakedDocsRoot)) {
    cpSync(bakedDocsRoot, stagingDocs, { recursive: true });
  }

  // Clear the whole bundle-owned region before writing the new version. The
  // baked mounts come from the image's content-bundles sidecar (bundle mounts
  // only, never site-local docs); the new manifest's mounts are re-materialized
  // from its files below.
  const toClear = new Set<string>(bakedMounts.filter(isDocsPrefix));
  for (const mount of verified.manifest.mounts) {
    if (!isDocsPrefix(mount.urlPrefix)) {
      log(
        `content overlay: bundle ${JSON.stringify(verified.manifest.name)} mount ` +
          `${JSON.stringify(mount.urlPrefix)} is outside /docs and cannot be overlay-served; ` +
          `its files keep serving from baked content`,
      );
      continue;
    }
    toClear.add(mount.urlPrefix);
  }
  for (const prefix of toClear) {
    rmSync(join(staging, `.${prefix}`), { recursive: true, force: true });
  }

  for (const path of [...verified.files.keys()].sort()) {
    if (!path.startsWith('docs/')) continue; // outside /docs — warned per mount above
    const target = join(staging, path);
    mkdirSync(dirname(target), { recursive: true });
    writeFileSync(target, verified.files.get(path) as Uint8Array);
  }

  // Rebuild the search index over the unioned tree so overlay pages are
  // findable; served by the overlay-aware /search/index.json route.
  const index = buildSearchIndex(stagingDocs);
  mkdirSync(join(staging, 'search'), { recursive: true });
  writeFileSync(join(staging, 'search', 'index.json'), JSON.stringify(index));
}

/**
 * Fetch, verify, materialize and activate one bundle version. Every outcome —
 * success or rejection — is logged and returned; nothing throws toward the
 * caller's serving path.
 */
export async function ingestBundle(options: IngestOptions): Promise<IngestOutcome> {
  const { bundle, digest, bakedDocsRoot, sizeCapBytes } = options;
  const log = options.log ?? defaultLog;
  const label = `content overlay: bundle ${JSON.stringify(bundle.name)}@${digest.slice(0, 12)}…`;
  const rejected = (outcome: IngestOutcomeKind, reason: string): IngestOutcome => {
    log(`${label}: ${reason} — keeping current content`);
    return { outcome, activeDigest: activeContentDigest(), reason };
  };

  if (digest === activeContentDigest()) {
    return { outcome: 'unchanged', activeDigest: digest };
  }
  if (!validDigest(digest)) {
    return rejected('rejected-digest', `target ${JSON.stringify(digest)} is not a sha256 bare-hex digest`);
  }

  const fetchBlob =
    options.fetchBlob ?? ((b: Pick<LockBundle, 'package' | 'digest' | 'registry'>) => fetchBundleBlobFromRegistry(b));
  let blob: Uint8Array;
  try {
    blob = await fetchBlob({ package: bundle.package, digest, registry: bundle.registry });
  } catch (error) {
    return rejected('fetch-failed', `blob fetch failed: ${(error as Error).message}`);
  }

  if (blob.length > sizeCapBytes) {
    return rejected('rejected-size', `blob of ${blob.length} bytes exceeds the ${sizeCapBytes}-byte overlay cap`);
  }
  const got = sha256Hex(blob);
  if (got !== digest) {
    return rejected('rejected-digest', `fetched blob digest ${got} does not match the channel pointer digest`);
  }

  const archive = verifyBundleArchive(blob);
  if (archive.newerFormatMajor) {
    // The deliberate "force a CI/CD cycle" signal: a newer format MAJOR is
    // ignored at runtime, baked content keeps serving (a newer MINOR passes).
    return rejected('rejected-format', formatDiagnostics(archive.diagnostics));
  }
  if (archive.bundle === null) {
    return rejected('rejected-digest', `invalid bundle: ${formatDiagnostics(archive.diagnostics)}`);
  }
  const verified = archive.bundle;
  if (verified.manifest.name !== bundle.name) {
    return rejected(
      'rejected-digest',
      `manifest name ${JSON.stringify(verified.manifest.name)} does not match the lock entry name`,
    );
  }
  if (verified.manifest.mounts.some((m) => m.urlPrefix === '/docs')) {
    return rejected('rejected-digest', `mount "/docs" would shadow the baked docs root`);
  }

  const versionsDir = options.versionsDir ?? overlayVersionsDir();
  const versionDir = join(versionsDir, digest);
  if (!isVersionComplete(versionDir)) {
    const staging = join(versionsDir, `.staging-${digest}`);
    try {
      rmSync(staging, { recursive: true, force: true });
      mkdirSync(staging, { recursive: true });
      materializeVersion(staging, bakedDocsRoot, options.bakedMounts ?? [], verified, log);
      // Written last, inside staging: the marker travels with the tree through
      // the rename below, so it can never label a partial materialization.
      writeFileSync(join(staging, VERSION_COMPLETE_MARKER), `${digest}\n`);
      // Drop whatever occupies the final path first. Reaching here means it is
      // NOT a complete version — a reaped skeleton, or a directory written
      // before the marker existed — and `rename(2)` refuses a non-empty target.
      rmSync(versionDir, { recursive: true, force: true });
      // Atomic on-disk finalize: only a COMPLETE version ever exists at its
      // final path. The in-memory pointer flip below is the reader-visible swap.
      renameSync(staging, versionDir);
    } catch (error) {
      rmSync(staging, { recursive: true, force: true });
      if (!isVersionComplete(versionDir)) {
        return rejected('fetch-failed', `materialization failed: ${(error as Error).message}`);
      }
      // A concurrent writer finalized the same digest first — content-addressed
      // dirs are interchangeable, adopt theirs (first-writer-wins).
    }
  }

  setActive(join(versionDir, 'docs'), digest);
  try {
    await (options.onSwap ?? defaultOnSwap)();
  } catch (error) {
    // The swap already happened; nav/ISR caches self-heal on their TTLs.
    log(`${label}: post-swap invalidation failed: ${(error as Error).message}`);
  }
  log(`${label}: activated (active content digest ${digest})`);
  return { outcome: 'updated', activeDigest: digest };
}
