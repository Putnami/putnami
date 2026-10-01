import { mkdir, rm, writeFile } from 'node:fs/promises';
import { robustRemove, robustRename } from '@putnami/runtime/robustio';
import { joinPath } from '@putnami/utils';
import type { BucketDefinition, StorageAccess } from './bucket/bucket.types';

/** Current infra-requirements protocol version (see protocols/infra). */
const PROTOCOL_VERSION = 2;

/** JSON Schema reference written into the emitted per-project manifest. */
const SCHEMA_URL = 'https://putnami.dev/schemas/putnami-infra.json';

/**
 * Per-producer scratch slug. Each framework producer owns one file at
 * `<project>/.gen/infra/<slug>.json`; the TypeScript generator syncs all
 * fragments into committed `infra/requirements.json`.
 */
const SIDECAR_SLUG = 'storage';
const SIDECAR_RELATIVE_PATH = `.gen/infra/${SIDECAR_SLUG}.json`;

/** A single object-storage bucket requirement (matches the infra schema). */
export interface StorageBucketRequirement {
  readonly name: string;
  readonly access?: StorageAccess;
  readonly public?: boolean;
  readonly retention?: string;
}

/** The subset of the per-project infra manifest emitted by @putnami/storage. */
export interface StorageInfraManifest {
  readonly $schema: string;
  readonly protocolVersion: number;
  readonly storage: StorageBucketRequirement[];
}

/**
 * Build the per-project infra manifest from the registered buckets, or
 * `undefined` when there are no buckets to declare. Entries are sorted by name
 * so repeated generation is byte-deterministic. Access defaults to `readwrite`
 * (the backend the plugin provides reads and writes the bucket); `public` and
 * `retention` are omitted unless declared. Field order matches the Go producer
 * so both emit byte-identical JSON (see test/cross-language.test.ts).
 */
export function buildStorageInfraManifest(buckets: BucketDefinition[]): StorageInfraManifest | undefined {
  if (buckets.length === 0) {
    return undefined;
  }

  const storage = buckets
    .map((bucket): StorageBucketRequirement => {
      const requirement: { name: string; access: StorageAccess; public?: boolean; retention?: string } = {
        name: bucket.bucketName,
        access: bucket.options.access ?? 'readwrite',
      };
      if (bucket.options.public) {
        requirement.public = true;
      }
      const retention = bucket.options.retention?.trim();
      if (retention) {
        requirement.retention = retention;
      }
      return requirement;
    })
    .sort((a, b) => a.name.localeCompare(b.name));

  return {
    $schema: SCHEMA_URL,
    protocolVersion: PROTOCOL_VERSION,
    storage,
  };
}

/**
 * Write the framework-generated infra scratch fragment for the given project from its
 * registered buckets.
 *
 * The write is atomic (temp file + rename) per the infra protocol contract: the
 * same generate task runs for both the build and test pipelines and generator
 * sync reads scratch fragments. When no buckets are registered, any stale
 * scratch fragment from a prior run is removed so sync never reads requirements
 * that no longer exist.
 *
 * @returns the sidecar path when written, otherwise `undefined`.
 */
export async function writeStorageInfraManifest(
  projectRoot: string,
  buckets: BucketDefinition[],
): Promise<string | undefined> {
  const sidecarPath = joinPath(projectRoot, SIDECAR_RELATIVE_PATH);
  const manifest = buildStorageInfraManifest(buckets);

  if (!manifest) {
    await robustRemove(sidecarPath);
    return undefined;
  }

  await mkdir(joinPath(projectRoot, '.gen/infra'), { recursive: true });
  const data = `${JSON.stringify(manifest, null, 2)}\n`;
  // Unique temp name per write so the build and test generate() tasks — which
  // run concurrently against the same sidecar — never share or clobber a temp
  // file or race on rename().
  const tmpPath = `${sidecarPath}.${process.pid}.${crypto.randomUUID()}.tmp`;
  try {
    await writeFile(tmpPath, data, 'utf8');
    await robustRename(tmpPath, sidecarPath);
  } catch (error) {
    // Best-effort cleanup: it must not replace the write error.
    await rm(tmpPath, { force: true }).catch(() => undefined);
    throw error;
  }
  return sidecarPath;
}
