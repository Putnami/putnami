const BUCKET_MARKER = 'putnami:bucket' as const;

// ---- Access ----

/**
 * Access level a workload needs to a bucket. Mirrors the storage protocol's
 * closed enum (go.putnami.dev/protocol/storage) and drives the IAM grant a
 * deploy target provisions for the runtime identity. Surfaced as the
 * infra.storage requirement's `access` field so the role travels with the
 * declared requirements.
 */
export type StorageAccess = 'read' | 'write' | 'readwrite';

// ---- Bucket Options ----

export interface BucketOptions {
  /** Maximum file size (e.g., '10mb', '1gb'). Validated on upload. */
  readonly maxFileSize?: string;
  /** Allowed MIME types (e.g., ['image/png', 'application/pdf']). Validated on upload. */
  readonly allowedMimeTypes?: readonly string[];
  /** Whether objects in this bucket are publicly readable. Defaults to false. */
  readonly public?: boolean;
  /**
   * Access level the workload needs; surfaced as the infra.storage requirement's
   * `access` field so a deploy target grants the runtime identity that role.
   * Defaults to 'readwrite' when the requirement is emitted, since the backend
   * the plugin provides reads and writes the bucket.
   */
  readonly access?: StorageAccess;
  /** Named storage backend (e.g., 'primary', 'archive'). Uses default when omitted. */
  readonly storage?: string;
  /**
   * Free-form object retention hint surfaced to deployers via the infra
   * requirements manifest (e.g., '30d'). Format is deployer-defined.
   */
  readonly retention?: string;
}

// ---- Bucket Definition ----

export interface BucketDefinition<O extends BucketOptions = BucketOptions> {
  readonly __bucket: typeof BUCKET_MARKER;
  readonly bucketName: string;
  readonly options: O;
  /** @internal Native declaration location for build-time design discovery. */
  readonly __source?: { path: string; line?: number; symbol?: string };
}

// ---- Type Guards ----

export function isBucketDefinition(value: unknown): value is BucketDefinition {
  return typeof value === 'object' && value !== null && (value as BucketDefinition).__bucket === BUCKET_MARKER;
}

// ---- Internal marker export ----

export { BUCKET_MARKER };
