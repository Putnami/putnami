import { validateKey } from '../backend/key.utils';
import type { StorageBackend } from '../backend/storage.backend';
import type { BucketDefinition } from '../bucket/bucket.types';
import { BucketHelper } from '../bucket/bucket.helper';
import { type StorageMetrics, recordSlowStorageOp, recordStorageError, recordStorageOp } from '../observability';
import type {
  GetResult,
  ListOptions,
  ListResult,
  ObjectMetadata,
  PutResult,
  SignOptions,
  SignedUrl,
  StorageOperation,
} from './storage.types';

/**
 * High-level storage client for a specific bucket.
 *
 * Wraps a StorageBackend with:
 * - Object-key validation (path traversal, null bytes, absolute paths) at the
 *   public boundary, so every backend inherits it regardless of configuration
 * - Bucket constraint validation (max file size, allowed MIME types)
 * - Observability (metrics, slow operation warnings)
 * - Consistent error handling
 *
 * @example
 * ```typescript
 * const avatars = await storage('avatars');
 * await avatars.put('user-123/photo.png', file, { contentType: 'image/png' });
 * const url = avatars.url('user-123/photo.png');
 * const data = await avatars.get('user-123/photo.png');
 * await avatars.delete('user-123/photo.png');
 * ```
 */
export class StorageClient {
  private helper: BucketHelper<BucketDefinition>;

  constructor(
    bucketDef: BucketDefinition,
    private backend: StorageBackend,
    private slowThresholdMs: number = 0,
  ) {
    this.helper = new BucketHelper(bucketDef);
  }

  /** The bucket name */
  get bucketName(): string {
    return this.helper.bucketName;
  }

  /** Whether the bucket is public */
  get isPublic(): boolean {
    return this.helper.isPublic;
  }

  /**
   * Upload an object to this bucket.
   * Validates file size and MIME type constraints before uploading.
   */
  async put(key: string, data: Blob | Buffer | ReadableStream, metadata?: ObjectMetadata): Promise<PutResult> {
    validateKey(key);

    let payload: Blob | Buffer | ReadableStream = data;

    // Validate constraints if we can determine size/type upfront
    if (data instanceof Blob) {
      const errors = this.helper.validate({ size: data.size, mimeType: metadata?.contentType ?? data.type });
      if (errors.length > 0) throw new StorageValidationError(errors);
    } else if (Buffer.isBuffer(data)) {
      const errors = this.helper.validate({ size: data.length, mimeType: metadata?.contentType });
      if (errors.length > 0) throw new StorageValidationError(errors);
    } else if (data instanceof ReadableStream) {
      // A stream has no size up front: validate the declared MIME type now
      // and cap the byte count as the stream is consumed, so a streamed upload
      // cannot exceed the bucket's size limit (or fill the disk).
      const errors = this.helper.validate({ size: 0, mimeType: metadata?.contentType });
      if (errors.length > 0) throw new StorageValidationError(errors);

      const maxBytes = this.helper.maxFileSizeBytes;
      if (maxBytes !== undefined) {
        payload = limitStreamSize(data, maxBytes, this.helper.maxFileSize ?? `${maxBytes} bytes`);
      }
    }

    return this.observe('put', key, () => this.backend.put(this.bucketName, key, payload, metadata));
  }

  /**
   * Download an object from this bucket.
   * Returns null if the object does not exist.
   */
  async get(key: string): Promise<GetResult | null> {
    validateKey(key);
    return this.observe('get', key, () => this.backend.get(this.bucketName, key));
  }

  /**
   * Delete an object from this bucket.
   */
  async delete(key: string): Promise<void> {
    validateKey(key);
    return this.observe('delete', key, () => this.backend.delete(this.bucketName, key));
  }

  /**
   * List objects in this bucket.
   */
  async list(options?: ListOptions): Promise<ListResult> {
    return this.observe('list', options?.prefix, () => this.backend.list(this.bucketName, options));
  }

  /**
   * Check if an object exists in this bucket.
   */
  async exists(key: string): Promise<boolean> {
    validateKey(key);
    return this.observe('exists', key, () => this.backend.exists(this.bucketName, key));
  }

  /**
   * Copy an object within this bucket.
   */
  async copy(source: string, destination: string): Promise<void> {
    validateKey(source);
    validateKey(destination);
    return this.observe('copy', source, () => this.backend.copy(this.bucketName, source, destination));
  }

  /**
   * Get a public URL for an object.
   * Meaningful for public buckets; for private buckets use signedDownloadUrl().
   */
  url(key: string): string {
    validateKey(key);
    return this.backend.url(this.bucketName, key);
  }

  /**
   * Generate a signed URL for direct upload from a client (browser).
   * The browser can PUT directly to this URL without going through the app server.
   * When the bucket declares allowed MIME types, `options.contentType` must be one of them.
   */
  async signedUploadUrl(key: string, options?: SignOptions): Promise<SignedUrl> {
    validateKey(key);
    const errors = this.helper.validate({ size: 0, mimeType: options?.contentType });
    if (errors.length > 0) throw new StorageValidationError(errors);
    return this.observe('signedUploadUrl', key, () => this.backend.signedUploadUrl(this.bucketName, key, options));
  }

  /**
   * Generate a signed URL for direct download from a client (browser).
   * Useful for private buckets where objects aren't publicly accessible.
   */
  async signedDownloadUrl(key: string, options?: SignOptions): Promise<SignedUrl> {
    validateKey(key);
    return this.observe('signedDownloadUrl', key, () => this.backend.signedDownloadUrl(this.bucketName, key, options));
  }

  // ---- Observability ----

  private async observe<R>(operation: StorageOperation, key: string | undefined, fn: () => Promise<R>): Promise<R> {
    const start = Date.now();
    const bucket = this.bucketName;

    try {
      const result = await fn();
      const duration = Date.now() - start;
      const metrics: StorageMetrics = { operation, bucket, duration, key };
      recordStorageOp(metrics);

      if (this.slowThresholdMs > 0) {
        recordSlowStorageOp(metrics, this.slowThresholdMs);
      }

      return result;
    } catch (error) {
      const duration = Date.now() - start;
      recordStorageError(operation, bucket, duration, error);
      throw error;
    }
  }
}

// ---- Stream size enforcement ----

const chunkByteLength = (chunk: unknown): number => {
  if (chunk instanceof Uint8Array) return chunk.byteLength;
  if (chunk instanceof ArrayBuffer) return chunk.byteLength;
  if (typeof chunk === 'string') return Buffer.byteLength(chunk);
  return 0;
};

/**
 * Wrap a ReadableStream so it errors as soon as more than `maxBytes` have flowed
 * through it, bounding memory and disk use for uploads whose size is not known
 * up front (e.g. request-body streams). The backend aborts and cleans up the
 * partial write when the stream errors.
 */
function limitStreamSize(stream: ReadableStream, maxBytes: number, maxLabel: string): ReadableStream {
  let total = 0;
  const limiter = new TransformStream({
    transform(chunk, controller) {
      total += chunkByteLength(chunk);
      if (total > maxBytes) {
        controller.error(new StorageValidationError([`File size exceeds maximum ${maxLabel}`]));
        return;
      }
      controller.enqueue(chunk);
    },
  });
  return stream.pipeThrough(limiter);
}

// ---- Errors ----

export class StorageValidationError extends Error {
  constructor(public errors: string[]) {
    super(`Storage validation failed: ${errors.join('; ')}`);
    this.name = 'StorageValidationError';
  }
}
