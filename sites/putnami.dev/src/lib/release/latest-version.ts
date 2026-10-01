/**
 * The Putnami release the site advertises: the version the `latest` channel of
 * the CLI points at on the registry. Production follows `latest` too
 * (`envs.prod` in `putnami.ci.json`), so it is also the release the site runs.
 *
 * The site cannot show its own build version for this. A deployed image runs
 * under the release id Cloud injects as `PUTNAMI_VERSION` (`cm_<hex>`), which
 * names a deployment, not a Putnami release. The channel pointer is the
 * public answer to "which Putnami is current": `putnami upgrade` installs the
 * same version.
 *
 * The lookup runs at request time, never at build: a build that read the
 * network would bake whatever the registry said at that moment into cached
 * generation output. It is TTL-cached and single-flight per process, and a
 * failed lookup keeps serving the last version it resolved.
 */

const DEFAULT_REGISTRY_URL = 'https://put.putnami.dev';

/** Channel and package the footer reports. */
export const RELEASE_CHANNEL = 'latest';
const CLI_POINTER_PATH = `putnami/cli/channels/${RELEASE_CHANNEL}`;

/** How long a resolved version is served before the next lookup. */
export const LATEST_VERSION_TTL_MS = 5 * 60 * 1000;

/** A lookup that has not answered in this time counts as failed. */
const LOOKUP_TIMEOUT_MS = 3000;

/** A release version: `1.2.3`, optionally with a pre-release or build suffix. */
const RELEASE_VERSION = /^\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.-]+)?$/;

export type PointerFetch = (url: string, init: { signal: AbortSignal }) => Promise<Response>;

export interface LatestVersionOptions {
  /** Registry base URL; defaults to `PUTNAMI_REGISTRY_URL`, then the public registry. */
  registryUrl?: string;
  /** Transport seam for tests. */
  fetch?: PointerFetch;
  /** Clock seam for the TTL (tests). */
  now?: () => number;
}

let cached: { version: string | undefined; at: number } | undefined;
let inFlight: Promise<string | undefined> | null = null;

/** Test helper: forget the cached version and any lookup in flight. */
export function resetLatestVersionForTest(): void {
  cached = undefined;
  inFlight = null;
}

function pointerUrl(registryUrl: string): string {
  return `${registryUrl.replace(/\/+$/, '')}/${CLI_POINTER_PATH}`;
}

async function lookup(options: LatestVersionOptions): Promise<string | undefined> {
  const registryUrl = options.registryUrl ?? (process.env['PUTNAMI_REGISTRY_URL'] || DEFAULT_REGISTRY_URL);
  const doFetch = options.fetch ?? ((url, init) => fetch(url, init));
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), LOOKUP_TIMEOUT_MS);
  try {
    const response = await doFetch(pointerUrl(registryUrl), { signal: controller.signal });
    if (!response.ok) return undefined;
    const doc = (await response.json()) as unknown;
    if (typeof doc !== 'object' || doc === null) return undefined;
    const version = (doc as Record<string, unknown>)['version'];
    return typeof version === 'string' && RELEASE_VERSION.test(version.trim()) ? version.trim() : undefined;
  } catch {
    return undefined;
  } finally {
    clearTimeout(timer);
  }
}

/**
 * The version `latest` points at, or `undefined` when the registry has never
 * answered with one. Within the TTL the cached answer is returned without a
 * lookup; concurrent callers share one lookup.
 */
export async function resolveLatestVersion(options: LatestVersionOptions = {}): Promise<string | undefined> {
  const now = options.now ?? Date.now;
  if (cached && now() - cached.at < LATEST_VERSION_TTL_MS) return cached.version;
  if (inFlight) return inFlight;

  inFlight = lookup(options)
    .then((version) => {
      cached = { version: version ?? cached?.version, at: now() };
      return cached.version;
    })
    .finally(() => {
      inFlight = null;
    });
  return inFlight;
}
