import { randomBytes } from 'node:crypto';
import { Config, Default, Int, Optional, Sensitive, useLogger } from '@putnami/runtime';

/**
 * Per-process fallback secret shared between the storage factory and storage server
 * when no explicit tokenSecret is configured. Generated once at module load.
 *
 * This value is stable within a single process so signed URLs minted by
 * `storage()` validate in `storageServer()`, but it is NOT stable across
 * replicas or restarts — see {@link resolveTokenSecret}.
 */
export const processTokenSecret = randomBytes(32).toString('hex');

/** Ensures the missing-secret warning is emitted at most once per process. */
let warnedAboutGeneratedSecret = false;

/**
 * Resolve the HMAC secret used to sign local file-backend tokens.
 *
 * Returns the configured `storage.tokenSecret` when set. When it is missing the
 * function falls back to the per-process {@link processTokenSecret} and warns
 * loudly: that fallback is regenerated on every process start, so a signed URL
 * minted by one replica (or before a restart) fails validation on another
 * replica (or after a restart). A stable, shared `tokenSecret` is mandatory for
 * any horizontally-scaled or persistent deployment.
 */
export function resolveTokenSecret(configuredSecret?: string): string {
  if (configuredSecret) return configuredSecret;
  if (!warnedAboutGeneratedSecret) {
    warnedAboutGeneratedSecret = true;
    useLogger('storage').warn(
      'storage.tokenSecret is not configured; falling back to a per-process random secret. ' +
        'Signed URLs will fail validation across replicas and after restarts. ' +
        'Set a stable, shared storage.tokenSecret for multi-instance or persistent deployments.',
    );
  }
  return processTokenSecret;
}

export const StorageConfig = Config('storage', {
  /** Backend type: 'remote', 'file', 'memory', or 's3' */
  backend: Default(String, 'file'),
  /** Endpoint for the remote backend (e.g., 'https://storage.putnami.cloud') */
  endpoint: Default(String, 'https://storage.putnami.cloud'),
  /**
   * Access key for the remote backend, sent as `Authorization: Bearer <accessKey>`.
   * The storage service authenticates this bearer token and signs URLs
   * server-side; there is no client-side secret-key request signing.
   */
  accessKey: Optional(Sensitive(String)),
  /** Data directory for the file backend */
  dataDir: Default(String, '.data/storage'),
  /** HMAC secret for signing local dev tokens. Shared between storage() and storageServer(). Auto-generated per-process when omitted. */
  tokenSecret: Optional(String),
  /** Operations slower than this threshold (ms) are logged at warn level. 0 disables. */
  slowOperationThresholdMs: Default(Int, 0),

  // ---- S3 backend (native Bun.S3Client, client-side signing) ----
  /** S3 access key ID. Falls back to `S3_ACCESS_KEY_ID`/`AWS_ACCESS_KEY_ID`. */
  accessKeyId: Optional(String),
  /** S3 secret access key. Falls back to `S3_SECRET_ACCESS_KEY`/`AWS_SECRET_ACCESS_KEY`. */
  secretAccessKey: Optional(Sensitive(String)),
  /** S3 region. Falls back to `S3_REGION`/`AWS_REGION`, then `us-east-1`. */
  region: Optional(String),
  /** Session token for temporary (STS) credentials. */
  sessionToken: Optional(Sensitive(String)),
  /** S3-compatible endpoint override (Cloudflare R2, MinIO, GCS-S3). Omit for AWS S3. */
  s3Endpoint: Optional(String),
  /** Use virtual-hosted-style addressing instead of path-style (default). */
  virtualHostedStyle: Default(Boolean, false),
  /** Canonical ACL applied to objects written by the S3 backend (e.g. 'private', 'public-read'). */
  s3Acl: Optional(String),
  /** Public base URL used by `url()` for public S3 buckets (e.g. a CDN). The object key is appended. */
  publicBaseUrl: Optional(String),
});
