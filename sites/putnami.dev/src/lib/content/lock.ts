/**
 * `content.lock.json` — the committed pin for site-content bundles.
 *
 * Follows the artifact-store lock-integrity model: `digest` is the ONLY
 * authoritative fetch field (sha256 bare-hex of the bundle blob); `version` and
 * `channelHint` are human provenance recorded by `content bump`, never resolved
 * at build time. The lock participates in the generate cache key (declared in
 * `putnami.json` `filePatterns`), so same lock → same generate key and a lock
 * bump → a new key, by construction (a determinism constraint).
 *
 * Lock errors always throw — a committed lock that does not parse is a config
 * bug that must fail identically in CI and local dev.
 */
import { readFileSync } from 'node:fs';
import { validDigest } from './sitecontent';

export interface LockBundle {
  /** Stable logical bundle name (matches the manifest `name`). */
  name: string;
  /** put-registry package, an opaque "<namespace>/<package>" reference, e.g. "cloud/doc-contents-platform". */
  package: string;
  /** Channel `content bump` resolves from — provenance, never fetched. */
  channelHint: string;
  /** Resolved version at bump time — provenance, never fetched. */
  version: string;
  /** sha256 bare-hex of the bundle blob. The only authoritative fetch field. */
  digest: string;
  /** Registry base URL the blob is fetched from. */
  registry: string;
}

export interface ContentLock {
  bundles: LockBundle[];
}

function requireString(value: unknown, at: string): string {
  if (typeof value !== 'string' || value === '') {
    throw new Error(`content.lock.json: ${at} must be a non-empty string`);
  }
  return value;
}

/** Parse and validate lock text. Throws a descriptive error on any defect. */
export function parseContentLock(text: string): ContentLock {
  let doc: unknown;
  try {
    doc = JSON.parse(text);
  } catch (error) {
    throw new Error(`content.lock.json is not valid JSON: ${(error as Error).message}`);
  }
  if (typeof doc !== 'object' || doc === null || Array.isArray(doc)) {
    throw new Error('content.lock.json must be a JSON object');
  }
  const bundlesRaw = (doc as Record<string, unknown>)['bundles'];
  if (!Array.isArray(bundlesRaw)) {
    throw new Error('content.lock.json: "bundles" must be an array');
  }
  const seen = new Set<string>();
  const bundles = bundlesRaw.map((raw, i): LockBundle => {
    if (typeof raw !== 'object' || raw === null || Array.isArray(raw)) {
      throw new Error(`content.lock.json: bundles[${i}] must be an object`);
    }
    const record = raw as Record<string, unknown>;
    const bundle: LockBundle = {
      name: requireString(record['name'], `bundles[${i}].name`),
      package: requireString(record['package'], `bundles[${i}].package`),
      channelHint: requireString(record['channelHint'], `bundles[${i}].channelHint`),
      version: requireString(record['version'], `bundles[${i}].version`),
      digest: requireString(record['digest'], `bundles[${i}].digest`),
      registry: requireString(record['registry'], `bundles[${i}].registry`),
    };
    if (!validDigest(bundle.digest)) {
      throw new Error(
        `content.lock.json: bundles[${i}].digest must be sha256 bare-hex (64 lowercase hex chars), ` +
          `got ${JSON.stringify(bundle.digest)}`,
      );
    }
    if (seen.has(bundle.name)) {
      throw new Error(`content.lock.json: duplicate bundle name ${JSON.stringify(bundle.name)}`);
    }
    seen.add(bundle.name);
    return bundle;
  });
  return { bundles };
}

/** Read + parse the committed lock. A missing lock file throws. */
export function readContentLock(lockPath: string): ContentLock {
  let text: string;
  try {
    text = readFileSync(lockPath, 'utf8');
  } catch (error) {
    throw new Error(`cannot read ${lockPath}: ${(error as Error).message}`);
  }
  return parseContentLock(text);
}

/**
 * Serialize a lock with a stable field order and formatting, so `content bump`
 * rewrites produce minimal, reviewable diffs.
 */
export function formatContentLock(lock: ContentLock): string {
  const bundles = lock.bundles.map((b) => ({
    name: b.name,
    package: b.package,
    channelHint: b.channelHint,
    version: b.version,
    digest: b.digest,
    registry: b.registry,
  }));
  return `${JSON.stringify({ bundles }, null, 2)}\n`;
}
