import { type InferConfig, useConfig } from '@putnami/runtime';
import { FileBackend } from './backend/file.backend';
import { MemoryBackend } from './backend/memory.backend';
import { RemoteBackend } from './backend/remote.backend';
import { type S3Acl, S3Backend } from './backend/s3.backend';
import type { StorageBackend } from './backend/storage.backend';
import { bucketRegistry } from './bucket/bucket.registry';
import { StorageClient } from './client/storage.client';
import { StorageConfig, processTokenSecret, resolveTokenSecret } from './config';
import { recordClientClosed, recordClientCount, recordClientCreated } from './observability';

interface ManagedClientEntry {
  client: StorageClient;
  backendKey: string;
}

interface ManagedBackendEntry {
  backend: StorageBackend;
  refCount: number;
}

const clientRegistry = new Map<string, ManagedClientEntry>();
const backendRegistry = new Map<string, ManagedBackendEntry>();

/**
 * Build a backend instance from config without mutating global registries.
 */
function buildBackend(conf: InferConfig<typeof StorageConfig>): StorageBackend {
  switch (conf.backend) {
    case 'remote':
      return new RemoteBackend({
        endpoint: conf.endpoint,
        accessKey: conf.accessKey,
      });
    case 'file':
      return new FileBackend(conf.dataDir, resolveTokenSecret(conf.tokenSecret));
    case 'memory':
      return new MemoryBackend();
    case 's3':
      return new S3Backend({
        accessKeyId: conf.accessKeyId,
        secretAccessKey: conf.secretAccessKey,
        region: conf.region,
        endpoint: conf.s3Endpoint,
        sessionToken: conf.sessionToken,
        virtualHostedStyle: conf.virtualHostedStyle,
        acl: conf.s3Acl as S3Acl | undefined,
        publicBaseUrl: conf.publicBaseUrl,
      });
    default:
      throw new Error(`Unknown storage backend: "${conf.backend}". Expected "remote", "file", "memory", or "s3".`);
  }
}

function backendCacheKey(conf: InferConfig<typeof StorageConfig>): string {
  switch (conf.backend) {
    case 'remote':
      return JSON.stringify({
        backend: 'remote',
        endpoint: conf.endpoint,
        accessKey: conf.accessKey ?? null,
      });
    case 'file':
      return JSON.stringify({
        backend: 'file',
        dataDir: conf.dataDir,
        tokenSecret: conf.tokenSecret ?? processTokenSecret,
      });
    case 'memory':
      return JSON.stringify({ backend: 'memory' });
    case 's3':
      return JSON.stringify({
        backend: 's3',
        accessKeyId: conf.accessKeyId ?? null,
        secretAccessKey: conf.secretAccessKey ?? null,
        region: conf.region ?? null,
        endpoint: conf.s3Endpoint ?? null,
        sessionToken: conf.sessionToken ?? null,
        virtualHostedStyle: conf.virtualHostedStyle,
        acl: conf.s3Acl ?? null,
        publicBaseUrl: conf.publicBaseUrl ?? null,
      });
    default:
      return JSON.stringify({ backend: conf.backend });
  }
}

function acquireBackend(conf: InferConfig<typeof StorageConfig>): ManagedBackendEntry & { key: string } {
  const key = backendCacheKey(conf);
  const cached = backendRegistry.get(key);
  if (cached) {
    cached.refCount++;
    return { ...cached, key };
  }

  const entry = {
    backend: buildBackend(conf),
    refCount: 1,
  };
  backendRegistry.set(key, entry);
  return { ...entry, key };
}

async function releaseBackend(key: string): Promise<void> {
  const cached = backendRegistry.get(key);
  if (!cached) return;

  cached.refCount--;
  if (cached.refCount > 0) return;

  backendRegistry.delete(key);
  await cached.backend.close();
}

/**
 * Get or create a StorageClient for a named bucket.
 *
 * @param bucketName - Bucket name (must match a Bucket() definition)
 * @param config - Optional explicit config (overrides environment config)
 *
 * @example
 * ```typescript
 * const avatars = await storage('avatars');
 * await avatars.put('user-123/photo.png', file);
 * ```
 */
export const storage = async (
  bucketName: string,
  _config?: InferConfig<typeof StorageConfig>,
): Promise<StorageClient> => {
  // Only use cache when no explicit config override is provided.
  // When _config is given, the caller may want a different backend/endpoint
  // and silently returning the cached client would be incorrect.
  if (!_config) {
    const cached = clientRegistry.get(bucketName);
    if (cached) return cached.client;
  }

  const bucketDef = bucketRegistry.getByName(bucketName);
  if (!bucketDef) {
    throw new Error(
      `Bucket "${bucketName}" is not registered. Available buckets: ${bucketRegistry.getNames().join(', ') || '(none)'}`,
    );
  }

  // Named storage backend allows per-bucket config override (e.g., storage.avatars)
  const path = bucketDef.options.storage ? `storage.${bucketDef.options.storage}` : undefined;
  const conf = useConfig(StorageConfig, { path, confInit: _config });

  // Explicit overrides are treated as one-off unmanaged clients so they don't
  // mutate the shared registry or accidentally replace the default cached client.
  const managedBackend = _config ? undefined : acquireBackend(conf);
  const backend = managedBackend?.backend ?? buildBackend(conf);
  const client = new StorageClient(bucketDef, backend, conf.slowOperationThresholdMs);

  if (!_config && managedBackend) {
    clientRegistry.set(bucketName, { client, backendKey: managedBackend.key });
    recordClientCreated(bucketName);
    recordClientCount(clientRegistry.size);
  }

  return client;
};

/**
 * Close a specific storage client by bucket name.
 */
export const closeStorage = async (bucketName: string): Promise<void> => {
  const client = clientRegistry.get(bucketName);
  if (!client) return;
  clientRegistry.delete(bucketName);
  await releaseBackend(client.backendKey);
  recordClientClosed(bucketName);
  recordClientCount(clientRegistry.size);
};

/**
 * Close all storage clients and their backends.
 */
export const closeAllStorage = async (): Promise<void> => {
  clientRegistry.clear();
  const backends = Array.from(backendRegistry.values()).map((entry) => entry.backend);
  backendRegistry.clear();
  await Promise.all(backends.map((backend) => backend.close()));
  recordClientCount(0);
};

/**
 * Get a StorageClient from the registry without creating a new one.
 * Returns undefined if the client hasn't been created yet.
 */
export const getStorageClient = (bucketName: string): StorageClient | undefined =>
  clientRegistry.get(bucketName)?.client;
