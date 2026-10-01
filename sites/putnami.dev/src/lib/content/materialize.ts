/**
 * Generate-time materialization of lock-pinned site-content bundles.
 *
 * Deterministic by construction: the ONLY inputs are the committed
 * `content.lock.json` (part of the generate cache key via `putnami.json`
 * `filePatterns`) and the digest-addressed blobs it pins. No channel is ever
 * resolved here — `content bump` is the sole channel-resolution point
 * (a generate-cache determinism constraint). Same lock → the same
 * bytes under `.gen/public/docs`, byte for byte.
 *
 * Failure policy:
 * - lock defects and mount collisions: ALWAYS throw (deterministic config
 *   bugs; they reproduce identically everywhere, hiding them in dev would let
 *   dev and CI build different trees from the same commit);
 * - unfetchable / invalid bundles: throw in CI, warn + skip in local dev so an
 *   offline laptop still builds the site from baked content. NOTE: a dev-mode
 *   skip produces a bundle-less `.gen` under the same cache key a complete CI
 *   build uses — acceptable while the failure needs an offline machine with a
 *   cold blob cache, but do not widen the skip path without revisiting how the
 *   generate cache would observe it;
 * - a bundle with a NEWER format major: ALWAYS skip + warn (the contract
 *   mandates falling back to baked content, CI included).
 */
import { mkdirSync, readdirSync, readFileSync, renameSync, rmSync, unlinkSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileExists } from '@putnami/utils';
import { type ContentLock, type LockBundle, readContentLock } from './lock';
import { fetchBundleBlobFromRegistry } from './registry';
import { type Diagnostic, prefixesConflict } from './sitecontent';
import { sha256Hex, type VerifiedBundle, verifyBundleArchive } from './verify';

/** Injectable seam: how a bundle blob is fetched when the cache misses. */
export type BlobFetcher = (bundle: LockBundle) => Promise<Uint8Array>;

export interface MaterializeOptions {
  projectRoot: string;
  workspaceRoot: string;
  /** Lock file path; defaults to `<projectRoot>/content.lock.json`. */
  lockPath?: string;
  /** Digest-keyed blob cache; defaults to `<workspaceRoot>/.putnami/site-content/blobs`. */
  cacheDir?: string;
  /** CI/deploy mode (hard-fail policy); defaults to a non-empty `CI` env var. */
  ci?: boolean;
  /** Blob fetcher; defaults to the put-registry fetch-by-digest client. */
  fetchBlob?: BlobFetcher;
  /** Site-local URL prefixes; defaults to derivation from `putnami.json`. */
  siteLocalPrefixes?: string[];
  /** Warning sink; defaults to a stderr line writer. */
  log?: (message: string) => void;
}

export interface SkippedBundle {
  name: string;
  reason: string;
}

export interface MaterializeResult {
  /** dist-relative asset key → absolute generated path (schemas-plugin shape). */
  assets: Record<string, string>;
  /** Bundle names successfully mounted. */
  mounted: string[];
  /** Bundles skipped under the dev / format-major fallback policy. */
  skipped: SkippedBundle[];
}

/** The CI signal the codebase already uses (tooling/cli output/live.go). */
export function detectCI(env: Record<string, string | undefined> = process.env): boolean {
  return (env['CI'] ?? '') !== '';
}

interface AssetEntry {
  from: string;
  to: string;
}

/**
 * Site-local URL prefixes derived from `putnami.json` `generate.assets`
 * targets. The root `public/docs` copy is expanded to its top-level sections
 * (the source directory's children) because ordered sections are the ownership
 * unit of the docs tree: a bundle may add a NEW `/docs/<nn-section>` but may
 * never overlap a section the site already materializes. All other targets own
 * their prefix as declared. Local prefixes are opaque strings here (they may
 * legally carry characters a bundle mount cannot, e.g. "&"), only bundle
 * mounts are validated against the contract's mount grammar.
 */
export function deriveSiteLocalPrefixes(projectRoot: string, workspaceRoot: string): string[] {
  const configPath = join(projectRoot, 'putnami.json');
  const config = JSON.parse(readFileSync(configPath, 'utf8')) as {
    options?: { generate?: { assets?: AssetEntry[] } };
  };
  const prefixes: string[] = [];
  for (const entry of config.options?.generate?.assets ?? []) {
    if (!entry.to.startsWith('public/')) continue;
    const prefix = entry.to.slice('public'.length); // "public/docs" → "/docs"
    if (prefix === '/docs') {
      const sourceDir = join(workspaceRoot, `./${entry.from}`);
      if (!fileExists(sourceDir)) continue;
      for (const child of readdirSync(sourceDir).sort()) {
        if (child.startsWith('.')) continue;
        prefixes.push(`/docs/${child}`);
      }
    } else {
      prefixes.push(prefix);
    }
  }
  return prefixes;
}

// ---------------------------------------------------------------------------
// Digest-keyed blob cache (offline-safe once fetched)
// ---------------------------------------------------------------------------

function cachedBlobPath(cacheDir: string, digest: string): string {
  return join(cacheDir, digest);
}

/** A cached blob is trusted only if its bytes still hash to its name. */
function readCachedBlob(cacheDir: string, digest: string): Uint8Array | null {
  const path = cachedBlobPath(cacheDir, digest);
  if (!fileExists(path)) return null;
  const blob = new Uint8Array(readFileSync(path));
  if (sha256Hex(blob) !== digest) {
    // Corrupted cache entry: drop it so the next build refetches.
    try {
      unlinkSync(path);
    } catch {
      // best-effort cleanup
    }
    return null;
  }
  return blob;
}

/** Content-addressed write: temp file + rename so concurrent builds never
 * observe a partially written blob. */
function storeCachedBlob(cacheDir: string, digest: string, blob: Uint8Array): void {
  mkdirSync(cacheDir, { recursive: true });
  const tmp = cachedBlobPath(cacheDir, `.${digest}.tmp-${process.pid}-${Date.now()}`);
  writeFileSync(tmp, blob);
  renameSync(tmp, cachedBlobPath(cacheDir, digest));
}

// ---------------------------------------------------------------------------
// Materialization
// ---------------------------------------------------------------------------

/**
 * Sidecar recording the mounts materialized on the last run. It lives at the
 * `.gen` root (NOT under `public/`, so it is never served) and is
 * cache-captured/restored as part of `.gen`, so it always describes whatever
 * `.gen` currently holds — whether that came from a fresh generate or a cache
 * restore. Reading it lets the next run prune a mount the lock no longer pins
 * before `.gen` is re-captured (finding #1: `.gen` is additive and
 * GenerateResult.assets never prunes, so a dropped mount would otherwise ship).
 */
export function manifestPathFor(projectRoot: string): string {
  return join(projectRoot, '.gen', 'content-bundles.manifest.json');
}

export function readMaterializedMounts(manifestPath: string): string[] {
  if (!fileExists(manifestPath)) return [];
  try {
    const doc = JSON.parse(readFileSync(manifestPath, 'utf8')) as { mounts?: unknown };
    if (!Array.isArray(doc.mounts)) return [];
    return doc.mounts.filter((m): m is string => typeof m === 'string');
  } catch {
    // A corrupt sidecar is best-effort bookkeeping, not user config: treat it
    // as empty rather than failing the build.
    return [];
  }
}

function formatDiagnostics(diags: Diagnostic[]): string {
  return diags.map((d) => `${d.code}${d.field ? ` at ${d.field}` : ''}: ${d.message}`).join('; ');
}

class BundleSkip extends Error {}

async function obtainBlob(bundle: LockBundle, cacheDir: string, fetchBlob: BlobFetcher): Promise<Uint8Array> {
  const cached = readCachedBlob(cacheDir, bundle.digest);
  if (cached !== null) return cached;

  let blob: Uint8Array;
  try {
    blob = await fetchBlob(bundle);
  } catch (error) {
    throw new BundleSkip(`unfetchable and not in the local cache: ${(error as Error).message}`);
  }
  const got = sha256Hex(blob);
  if (got !== bundle.digest) {
    throw new BundleSkip(`fetched blob digest ${got} does not match the pinned digest ${bundle.digest}`);
  }
  storeCachedBlob(cacheDir, bundle.digest, blob);
  return blob;
}

function checkMountCollisions(bundle: LockBundle, mounts: string[], owners: { name: string; prefix: string }[]): void {
  for (const mount of mounts) {
    for (const owner of owners) {
      if (prefixesConflict(mount, owner.prefix)) {
        throw new Error(
          `content bundle ${JSON.stringify(bundle.name)}: mount ${JSON.stringify(mount)} overlaps ` +
            `${JSON.stringify(owner.prefix)} owned by ${owner.name}. Mounts partition the site URL space — ` +
            `equal, parent, or child prefixes are hard errors. Change the bundle mount or the site layout.`,
        );
      }
    }
  }
}

function writeBundleFiles(projectRoot: string, verified: VerifiedBundle, assets: Record<string, string>): void {
  // Clear each mount target first so files dropped by a lock bump do not
  // linger from a previous materialization (schemas-plugin lifecycle).
  for (const mount of verified.manifest.mounts) {
    rmSync(join(projectRoot, '.gen', 'public', `./${mount.urlPrefix}`), { recursive: true, force: true });
    rmSync(join(projectRoot, 'public', `./${mount.urlPrefix}`), { recursive: true, force: true });
  }
  // Deterministic write order (path-sorted) so repeated runs are identical.
  for (const path of [...verified.files.keys()].sort()) {
    const data = verified.files.get(path) as Uint8Array;
    const distRelative = join('public', path);

    // .gen/<distRelative> — the production build output (cache-captured).
    const genPath = join(projectRoot, '.gen', distRelative);
    mkdirSync(dirname(genPath), { recursive: true });
    writeFileSync(genPath, data);

    // public/<...> — local development (the staticFiles plugin serves public/).
    const publicPath = join(projectRoot, distRelative);
    mkdirSync(dirname(publicPath), { recursive: true });
    writeFileSync(publicPath, data);

    assets[distRelative] = genPath;
  }
}

/**
 * Materialize every lock-pinned bundle into `.gen/public/<mount>` (production
 * build output) and `public/<mount>` (local dev static serve), returning the
 * generated assets to register. An empty lock is a clean no-op.
 */
export async function materializeContentBundles(options: MaterializeOptions): Promise<MaterializeResult> {
  const { projectRoot, workspaceRoot } = options;
  const lockPath = options.lockPath ?? join(projectRoot, 'content.lock.json');
  const ci = options.ci ?? detectCI();
  // Warnings go to stderr: the generate hook's stdout is a JSONL event stream,
  // and a dev-mode skip must never be silent (fail-closed visibility).
  const log = options.log ?? ((message: string) => process.stderr.write(`${message}\n`));

  const lock: ContentLock = readContentLock(lockPath);
  const result: MaterializeResult = { assets: {}, mounted: [], skipped: [] };

  // Cross-run cleanup (finding #1). Prune any mount from a previous run that
  // the current lock will not re-materialize, so a bundle dropped from the lock
  // does not linger in `.gen/public` (and the packaged dist) and keep shipping.
  // Nothing recorded AND an empty lock ⇒ a true no-op (never touches `.gen`).
  const manifestPath = manifestPathFor(projectRoot);
  const prevMounts = readMaterializedMounts(manifestPath);
  if (prevMounts.length === 0 && lock.bundles.length === 0) return result;

  const siteLocal = options.siteLocalPrefixes ?? deriveSiteLocalPrefixes(projectRoot, workspaceRoot);

  // A valid manifest only ever holds bundle mounts (validated non-overlapping
  // with site-local content), so this never deletes baked docs; the guard is
  // defence in depth against a hand-edited or corrupted sidecar.
  for (const prefix of prevMounts) {
    if (siteLocal.some((local) => prefixesConflict(local, prefix))) continue;
    rmSync(join(projectRoot, '.gen', 'public', `./${prefix}`), { recursive: true, force: true });
    rmSync(join(projectRoot, 'public', `./${prefix}`), { recursive: true, force: true });
  }

  const materializedMounts: string[] = [];
  const writeManifest = (): void => {
    // Only write when there is state to record or state we just pruned, so the
    // pure empty→empty path stays a clean no-op. Sorted for a deterministic,
    // cache-stable sidecar.
    if (materializedMounts.length === 0 && prevMounts.length === 0) return;
    mkdirSync(dirname(manifestPath), { recursive: true });
    writeFileSync(manifestPath, `${JSON.stringify({ mounts: [...materializedMounts].sort() }, null, 2)}\n`);
  };

  if (lock.bundles.length === 0) {
    writeManifest();
    return result;
  }

  const cacheDir = options.cacheDir ?? join(workspaceRoot, '.putnami', 'site-content', 'blobs');
  const fetchBlob = options.fetchBlob ?? ((bundle: LockBundle) => fetchBundleBlobFromRegistry(bundle));

  const owners = siteLocal.map((prefix) => ({ name: 'the site', prefix }));

  // Sequential, in lock order: deterministic collision reporting and writes.
  for (const bundle of lock.bundles) {
    let verified: VerifiedBundle;
    try {
      const blob = await obtainBlob(bundle, cacheDir, fetchBlob);
      const archive = verifyBundleArchive(blob);
      if (archive.newerFormatMajor) {
        // Contract-mandated fallback, CI included: a newer major is not an
        // error, the baked content simply keeps serving.
        log(
          `content bundle ${JSON.stringify(bundle.name)}: ${formatDiagnostics(archive.diagnostics)} — ` +
            `falling back to baked content`,
        );
        result.skipped.push({ name: bundle.name, reason: 'newer format major' });
        continue;
      }
      if (archive.bundle === null) {
        throw new BundleSkip(`invalid bundle: ${formatDiagnostics(archive.diagnostics)}`);
      }
      verified = archive.bundle;
      if (verified.manifest.name !== bundle.name) {
        throw new BundleSkip(
          `manifest name ${JSON.stringify(verified.manifest.name)} does not match the lock entry name`,
        );
      }
    } catch (error) {
      if (!(error instanceof BundleSkip)) throw error;
      const message = `content bundle ${JSON.stringify(bundle.name)} (pinned ${bundle.digest.slice(0, 12)}…): ${error.message}`;
      if (ci) {
        throw new Error(
          `${message}. The lock pins this bundle, so CI/deploy builds must fail rather than ship without it.`,
        );
      }
      log(`${message} — skipping in local dev (baked content keeps serving)`);
      result.skipped.push({ name: bundle.name, reason: error.message });
      continue;
    }

    // Collisions are config bugs: always hard errors, in dev too.
    const mounts = verified.manifest.mounts.map((m) => m.urlPrefix);
    checkMountCollisions(bundle, mounts, owners);
    for (const prefix of mounts) {
      owners.push({ name: `bundle ${JSON.stringify(bundle.name)}`, prefix });
    }

    writeBundleFiles(projectRoot, verified, result.assets);
    materializedMounts.push(...mounts);
    result.mounted.push(bundle.name);
  }

  writeManifest();
  return result;
}
