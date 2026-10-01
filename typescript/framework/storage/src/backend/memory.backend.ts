import type {
  GetResult,
  ListOptions,
  ListResult,
  ObjectMetadata,
  PutResult,
  SignOptions,
  SignedUrl,
} from '../client/storage.types';
import { StorageError } from '../errors';
import { validateKey } from './key.utils';
import type { StorageBackend } from './storage.backend';

interface StoredObject {
  key: string;
  data: Uint8Array;
  contentType?: string;
  metadata?: Record<string, string>;
  cacheControl?: string;
  contentDisposition?: string;
  etag: string;
  lastModified: Date;
}

/**
 * StorageBackend implementation that stores everything in memory.
 * Designed for tests — fast, isolated, no I/O.
 *
 * Keys are validated exactly as the file, S3, and remote backends validate
 * them, even though nothing here touches a filesystem or a URL. A key this
 * backend accepted but the others rejected would make a test pass against a
 * substitution that fails in production, which is the divergence the shared
 * backend contract exists to prevent.
 *
 * @example
 * ```typescript
 * const backend = new MemoryBackend();
 * const client = new StorageClient('test-bucket', backend);
 * // ... run tests ...
 * backend.clear(); // reset between tests
 * ```
 */
export class MemoryBackend implements StorageBackend {
  /** bucket -> key -> StoredObject */
  private store = new Map<string, Map<string, StoredObject>>();

  async put(
    bucket: string,
    key: string,
    data: Blob | Buffer | ReadableStream,
    metadata?: ObjectMetadata,
  ): Promise<PutResult> {
    validateKey(key);
    let bytes: Uint8Array;
    if (data instanceof Blob) {
      bytes = new Uint8Array(await data.arrayBuffer());
    } else if (Buffer.isBuffer(data)) {
      bytes = new Uint8Array(data);
    } else {
      const chunks: Uint8Array[] = [];
      const reader = data.getReader();
      while (true) {
        const { done, value } = await reader.read();
        if (done) break;
        chunks.push(value);
      }
      const totalLength = chunks.reduce((sum, c) => sum + c.length, 0);
      bytes = new Uint8Array(totalLength);
      let offset = 0;
      for (const chunk of chunks) {
        bytes.set(chunk, offset);
        offset += chunk.length;
      }
    }

    const etag = Bun.hash(bytes).toString(16);

    if (!this.store.has(bucket)) {
      this.store.set(bucket, new Map());
    }

    this.store.get(bucket)?.set(key, {
      key,
      data: bytes,
      contentType: metadata?.contentType,
      metadata: metadata?.custom,
      cacheControl: metadata?.cacheControl,
      contentDisposition: metadata?.contentDisposition,
      etag,
      lastModified: new Date(),
    });

    return { key, size: bytes.length, etag };
  }

  async get(bucket: string, key: string): Promise<GetResult | null> {
    validateKey(key);
    const obj = this.store.get(bucket)?.get(key);
    if (!obj) return null;

    return {
      key,
      body: new ReadableStream({
        start(controller) {
          controller.enqueue(obj.data);
          controller.close();
        },
      }),
      size: obj.data.length,
      contentType: obj.contentType,
      etag: obj.etag,
      lastModified: obj.lastModified,
      metadata: obj.metadata,
    };
  }

  async delete(bucket: string, key: string): Promise<void> {
    validateKey(key);
    this.store.get(bucket)?.delete(key);
  }

  async list(bucket: string, options?: ListOptions): Promise<ListResult> {
    const bucketStore = this.store.get(bucket);
    if (!bucketStore) {
      return { objects: [], prefixes: [], isTruncated: false };
    }

    const prefix = options?.prefix ?? '';
    const delimiter = options?.delimiter;
    const maxKeys = options?.maxKeys ?? 1000;

    const allKeys = [...bucketStore.keys()].filter((k) => k.startsWith(prefix)).sort();

    if (!delimiter) {
      const objects = allKeys.slice(0, maxKeys).map((key) => {
        const obj = bucketStore.get(key)!;
        return {
          key,
          size: obj.data.length,
          contentType: obj.contentType,
          etag: obj.etag,
          lastModified: obj.lastModified,
        };
      });
      return {
        objects,
        prefixes: [],
        isTruncated: allKeys.length > maxKeys,
        continuationToken: allKeys.length > maxKeys ? String(maxKeys) : undefined,
      };
    }

    const prefixSet = new Set<string>();
    const directObjects: string[] = [];

    for (const key of allKeys) {
      const rest = key.slice(prefix.length);
      const delimIdx = rest.indexOf(delimiter);
      if (delimIdx >= 0) {
        prefixSet.add(prefix + rest.slice(0, delimIdx + delimiter.length));
      } else {
        directObjects.push(key);
      }
    }

    const objects = directObjects.slice(0, maxKeys).map((key) => {
      const obj = bucketStore.get(key)!;
      return {
        key,
        size: obj.data.length,
        contentType: obj.contentType,
        etag: obj.etag,
        lastModified: obj.lastModified,
      };
    });

    return {
      objects,
      prefixes: [...prefixSet].sort(),
      isTruncated: false,
    };
  }

  async exists(bucket: string, key: string): Promise<boolean> {
    validateKey(key);
    return this.store.get(bucket)?.has(key) ?? false;
  }

  async copy(bucket: string, source: string, destination: string): Promise<void> {
    validateKey(source);
    validateKey(destination);
    const obj = this.store.get(bucket)?.get(source);
    if (!obj) throw new StorageError(`Source object not found: ${bucket}/${source}`, 'NOT_FOUND');

    if (!this.store.has(bucket)) {
      this.store.set(bucket, new Map());
    }

    this.store.get(bucket)?.set(destination, {
      ...obj,
      key: destination,
      lastModified: new Date(),
    });
  }

  url(bucket: string, key: string): string {
    validateKey(key);
    return `memory://${bucket}/${key}`;
  }

  async signedUploadUrl(bucket: string, key: string, _options?: SignOptions): Promise<SignedUrl> {
    validateKey(key);
    const expiresAt = new Date(Date.now() + 15 * 60 * 1000); // 15 minutes
    return {
      url: `memory://${bucket}/${key}?upload=true`,
      expiresAt,
      method: 'PUT',
    };
  }

  async signedDownloadUrl(bucket: string, key: string, _options?: SignOptions): Promise<SignedUrl> {
    validateKey(key);
    const expiresAt = new Date(Date.now() + 15 * 60 * 1000);
    return {
      url: `memory://${bucket}/${key}?download=true`,
      expiresAt,
      method: 'GET',
    };
  }

  async close(): Promise<void> {
    this.store.clear();
  }

  // ---- Test Utilities ----

  /** Clear all stored objects (useful between tests) */
  clear(): void {
    this.store.clear();
  }

  /** Get the number of objects in a bucket */
  objectCount(bucket: string): number {
    return this.store.get(bucket)?.size ?? 0;
  }

  /** Get all bucket names that have objects */
  bucketNames(): string[] {
    return [...this.store.keys()];
  }
}
