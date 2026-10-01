import type {
  GetResult,
  ListOptions,
  ListResult,
  ObjectMetadata,
  PutResult,
  SignOptions,
  SignedUrl,
} from '../client/storage.types';
import type { StorageError } from '../errors';

/**
 * StorageBackend defines the contract that all storage implementations must fulfill.
 *
 * Implementations:
 * - `RemoteBackend`  — HTTP calls to storage.putnami.cloud (production)
 * - `FileBackend`    — Local filesystem storage (local development)
 * - `MemoryBackend`  — In-memory storage (tests)
 */
export interface StorageBackend {
  /** Upload an object to the bucket */
  put(bucket: string, key: string, data: Blob | Buffer | ReadableStream, metadata?: ObjectMetadata): Promise<PutResult>;

  /** Download an object from the bucket. Returns null if not found. */
  get(bucket: string, key: string): Promise<GetResult | null>;

  /** Delete an object from the bucket */
  delete(bucket: string, key: string): Promise<void>;

  /** List objects in the bucket */
  list(bucket: string, options?: ListOptions): Promise<ListResult>;

  /** Check if an object exists */
  exists(bucket: string, key: string): Promise<boolean>;

  /**
   * Copy an object within the same bucket. Throws {@link StorageError} with code
   * `'NOT_FOUND'` when the source object does not exist.
   */
  copy(bucket: string, source: string, destination: string): Promise<void>;

  /** Get a public URL for an object (only meaningful for public buckets) */
  url(bucket: string, key: string): string;

  /** Generate a signed URL for direct upload from a client (browser) */
  signedUploadUrl(bucket: string, key: string, options?: SignOptions): Promise<SignedUrl>;

  /** Generate a signed URL for direct download from a client (browser) */
  signedDownloadUrl(bucket: string, key: string, options?: SignOptions): Promise<SignedUrl>;

  /** Close/cleanup any resources held by this backend */
  close(): Promise<void>;
}
