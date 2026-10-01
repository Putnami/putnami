import type {
  GetResult,
  ListOptions,
  ListResult,
  ObjectInfo,
  ObjectMetadata,
  PutResult,
  SignOptions,
  SignedUrl,
} from '../client/storage.types';
import { StorageError } from '../errors';
import { DEFAULT_SIGNED_URL_EXPIRY, resolveExpiry } from './file.backend';
import { encodeKeyPath, validateKey } from './key.utils';
import type { StorageBackend } from './storage.backend';

/** Canonical ACL accepted by Bun's native S3 client. */
export type S3Acl = NonNullable<Bun.S3Options['acl']>;

/** Default timeout for each S3 operation (30 seconds), matching {@link RemoteBackend}. */
const DEFAULT_TIMEOUT_MS = 30_000;

export interface S3BackendOptions {
  /** Access key ID. Falls back to `S3_ACCESS_KEY_ID`/`AWS_ACCESS_KEY_ID` when omitted. */
  accessKeyId?: string;
  /** Secret access key. Falls back to `S3_SECRET_ACCESS_KEY`/`AWS_SECRET_ACCESS_KEY` when omitted. */
  secretAccessKey?: string;
  /** AWS region. Falls back to `S3_REGION`/`AWS_REGION`, then `us-east-1`. */
  region?: string;
  /**
   * S3-compatible endpoint override (Cloudflare R2, MinIO, GCS-S3, …). Omit for
   * AWS S3, where the region-derived host is used.
   */
  endpoint?: string;
  /** Session token for temporary (STS) credentials. */
  sessionToken?: string;
  /**
   * Use virtual-hosted-style addressing (`https://<bucket>.<host>/<key>`) instead
   * of path-style (`https://<host>/<bucket>/<key>`). Path-style is the default
   * because it is what MinIO and most self-hosted providers expect.
   */
  virtualHostedStyle?: boolean;
  /** Canonical ACL applied to every object written by this backend. */
  acl?: S3Acl;
  /**
   * Public base URL used by {@link S3Backend.url} for public buckets, e.g. a CDN
   * or the bucket's website endpoint. The object key is appended directly, so
   * point it at the bucket root.
   */
  publicBaseUrl?: string;
  /**
   * Timeout in milliseconds for each S3 operation. Defaults to 30 000 ms. Bun's
   * native S3 client exposes no per-request timeout, so every network op is
   * raced against this bound to stop a black-holed endpoint from hanging the
   * caller's pool forever (see {@link RemoteBackend}).
   */
  timeoutMs?: number;
}

/**
 * StorageBackend backed by Bun's native S3 client (`Bun.S3Client`).
 *
 * Unlike {@link RemoteBackend}, signing happens **client-side**: presigned URLs
 * are generated locally with AWS SigV4 (no `_sign/*` server round-trip), and all
 * operations talk directly to an S3-compatible target. The putnami bucket name
 * maps 1:1 to the S3 bucket name.
 *
 * Works against AWS S3 and any S3-compatible provider (Cloudflare R2, MinIO,
 * GCS-S3) via the `endpoint` override.
 *
 * Known limitations of Bun's native client (retested on Bun 1.4.0): it cannot persist
 * arbitrary user metadata (`x-amz-meta-*`) or a `Cache-Control` header, so
 * `metadata.custom`/`metadata.cacheControl` are dropped on `put` and never
 * surfaced by `get`. `metadata.contentType` and `metadata.contentDisposition`
 * are honored. See `doc/05-s3-backend.md`.
 */
export class S3Backend implements StorageBackend {
  private readonly client: Bun.S3Client;
  private readonly endpoint?: string;
  private readonly region: string;
  private readonly virtualHostedStyle: boolean;
  private readonly acl?: S3Acl;
  private readonly publicBaseUrl?: string;
  private readonly timeoutMs: number;

  constructor(options: S3BackendOptions = {}) {
    this.endpoint = options.endpoint;
    this.region = options.region ?? 'us-east-1';
    this.virtualHostedStyle = options.virtualHostedStyle ?? false;
    this.acl = options.acl;
    this.publicBaseUrl = options.publicBaseUrl;
    this.timeoutMs = options.timeoutMs ?? DEFAULT_TIMEOUT_MS;
    // Credentials live on a single client instance; the bucket is passed per
    // operation so one backend can serve every registered bucket.
    this.client = new Bun.S3Client({
      accessKeyId: options.accessKeyId,
      secretAccessKey: options.secretAccessKey,
      region: options.region,
      endpoint: options.endpoint,
      sessionToken: options.sessionToken,
      virtualHostedStyle: options.virtualHostedStyle,
    });
  }

  async put(
    bucket: string,
    key: string,
    data: Blob | Buffer | ReadableStream,
    metadata?: ObjectMetadata,
  ): Promise<PutResult> {
    validateKey(key);
    const size = await this.withTimeout(
      `put ${bucket}/${key}`,
      this.client.write(key, toS3Body(data), {
        bucket,
        type: metadata?.contentType,
        contentDisposition: metadata?.contentDisposition,
        acl: this.acl,
      }),
    );
    // Bun's write() does not return an ETag; a follow-up stat() recovers it so
    // PutResult.etag matches the other backends. Treat a missing ETag as
    // non-fatal — the write already succeeded.
    let etag: string | undefined;
    try {
      etag = normalizeEtag((await this.withTimeout(`stat ${bucket}/${key}`, this.client.stat(key, { bucket }))).etag);
    } catch {
      etag = undefined;
    }
    return { key, size, etag };
  }

  async get(bucket: string, key: string): Promise<GetResult | null> {
    validateKey(key);
    const file = this.client.file(key, { bucket });
    let info: Bun.S3Stats;
    try {
      info = await this.withTimeout(`get ${bucket}/${key}`, file.stat());
    } catch (err) {
      if (isNotFoundError(err)) return null;
      throw err;
    }
    return {
      key,
      body: withS3ReadTimeout(file.stream(), this.timeoutMs, `get ${bucket}/${key} body`),
      size: info.size,
      contentType: info.type || undefined,
      etag: normalizeEtag(info.etag),
      lastModified: info.lastModified,
      metadata: undefined,
    };
  }

  async delete(bucket: string, key: string): Promise<void> {
    validateKey(key);
    // S3 DELETE is idempotent — deleting a missing key still returns success.
    await this.withTimeout(`delete ${bucket}/${key}`, this.client.delete(key, { bucket }));
  }

  async list(bucket: string, options?: ListOptions): Promise<ListResult> {
    const response = await this.withTimeout(
      `list ${bucket}`,
      this.client.list(
        {
          prefix: options?.prefix,
          delimiter: options?.delimiter,
          maxKeys: options?.maxKeys,
          continuationToken: options?.continuationToken,
        },
        { bucket },
      ),
    );
    return mapS3ListResponse(response);
  }

  async exists(bucket: string, key: string): Promise<boolean> {
    validateKey(key);
    return this.withTimeout(`exists ${bucket}/${key}`, this.client.exists(key, { bucket }));
  }

  async copy(bucket: string, source: string, destination: string): Promise<void> {
    validateKey(source);
    validateKey(destination);
    const sourceFile = this.client.file(source, { bucket });
    if (!(await this.withTimeout(`copy ${bucket}/${source}`, sourceFile.exists()))) {
      throw new StorageError(`Source object not found: ${bucket}/${source}`, 'NOT_FOUND');
    }
    await this.withTimeout(
      `copy ${bucket}/${source} -> ${destination}`,
      this.client.write(destination, sourceFile, { bucket, acl: this.acl }),
    );
  }

  url(bucket: string, key: string): string {
    validateKey(key);
    return this.publicUrl(bucket, key);
  }

  async signedUploadUrl(bucket: string, key: string, options?: SignOptions): Promise<SignedUrl> {
    validateKey(key);
    const ms = resolveExpiry(options?.expiresIn ?? DEFAULT_SIGNED_URL_EXPIRY);
    const url = this.client.presign(key, {
      bucket,
      method: 'PUT',
      expiresIn: Math.floor(ms / 1000),
      type: options?.contentType,
      acl: this.acl,
    });
    return { url, expiresAt: new Date(Date.now() + ms), method: 'PUT' };
  }

  async signedDownloadUrl(bucket: string, key: string, options?: SignOptions): Promise<SignedUrl> {
    validateKey(key);
    const ms = resolveExpiry(options?.expiresIn ?? DEFAULT_SIGNED_URL_EXPIRY);
    const url = this.client.presign(key, {
      bucket,
      method: 'GET',
      expiresIn: Math.floor(ms / 1000),
      contentDisposition: options?.contentDisposition,
    });
    return { url, expiresAt: new Date(Date.now() + ms), method: 'GET' };
  }

  async close(): Promise<void> {
    // Bun.S3Client holds no persistent connections to release.
  }

  // ---- Internal Helpers ----

  /**
   * Race an S3 operation against the configured timeout. Bun's native S3 client
   * exposes no per-request timeout or abort signal, so a stalled endpoint would
   * otherwise leave the await pending forever and exhaust the caller's pool. The
   * loser of the race rejects with a typed {@link StorageError}; the winning
   * operation's promise is left to settle on its own (Bun has no handle to
   * cancel it), so this bounds the *caller* rather than the socket.
   */
  private withTimeout<T>(operation: string, promise: Promise<T>): Promise<T> {
    return Promise.race([
      promise,
      new Promise<never>((_, reject) => {
        const signal = AbortSignal.timeout(this.timeoutMs);
        signal.addEventListener(
          'abort',
          () => reject(new StorageError(`S3 operation timed out after ${this.timeoutMs}ms: ${operation}`, 'TIMEOUT')),
          { once: true },
        );
      }),
    ]);
  }

  /**
   * Build an unsigned object URL for a public bucket. Prefers an explicit
   * `publicBaseUrl`; otherwise derives the host from the endpoint (or the AWS
   * region) using the configured addressing style.
   */
  private publicUrl(bucket: string, key: string): string {
    const encodedKey = encodeKeyPath(key);
    if (this.publicBaseUrl) {
      return `${stripTrailingSlash(this.publicBaseUrl)}/${encodedKey}`;
    }
    const base = this.endpoint ? stripTrailingSlash(this.endpoint) : `https://s3.${this.region}.amazonaws.com`;
    if (this.virtualHostedStyle) {
      const parsed = new URL(base);
      return `${parsed.protocol}//${encodeURIComponent(bucket)}.${parsed.host}/${encodedKey}`;
    }
    return `${base}/${encodeURIComponent(bucket)}/${encodedKey}`;
  }
}

// ---- Helpers ----

/**
 * Map the contract's `put` data union onto a value Bun's S3 client accepts. A
 * `ReadableStream` is wrapped in a `Response` (the streaming body type the
 * client understands); a `Buffer` is narrowed to `Uint8Array`.
 */
function toS3Body(data: Blob | Buffer | ReadableStream): Blob | Uint8Array | Response {
  if (data instanceof ReadableStream) return new Response(data);
  if (Buffer.isBuffer(data)) return new Uint8Array(data);
  return data;
}

function withS3ReadTimeout(stream: ReadableStream, timeoutMs: number, operation: string): ReadableStream {
  const reader = stream.getReader();
  return new ReadableStream({
    async pull(controller) {
      let timer: ReturnType<typeof setTimeout> | undefined;
      try {
        const result = await Promise.race([
          reader.read(),
          new Promise<never>((_, reject) => {
            timer = setTimeout(
              () => reject(new StorageError(`S3 operation timed out after ${timeoutMs}ms: ${operation}`, 'TIMEOUT')),
              timeoutMs,
            );
          }),
        ]);
        if (timer !== undefined) clearTimeout(timer);

        if (result.done) {
          controller.close();
          return;
        }
        controller.enqueue(result.value);
      } catch (error) {
        if (timer !== undefined) clearTimeout(timer);
        reader.cancel(error).catch(() => undefined);
        controller.error(error);
      }
    },
    async cancel(reason) {
      await reader.cancel(reason);
    },
  });
}

/**
 * Translate a `Bun.S3ListObjectsResponse` (ListObjectsV2 shape) into the
 * backend-agnostic {@link ListResult}. Exported for unit testing without a live
 * S3 target.
 */
export function mapS3ListResponse(response: Bun.S3ListObjectsResponse): ListResult {
  const objects: ObjectInfo[] = (response.contents ?? []).map((object) => ({
    key: object.key,
    size: object.size ?? 0,
    etag: normalizeEtag(object.eTag),
    lastModified: object.lastModified ? new Date(object.lastModified) : undefined,
  }));
  return {
    objects,
    prefixes: (response.commonPrefixes ?? []).map((entry) => entry.prefix),
    isTruncated: response.isTruncated ?? false,
    continuationToken: response.nextContinuationToken,
  };
}

/** Strip the surrounding quotes S3 wraps around ETags (`"abc"` → `abc`). */
function normalizeEtag(etag?: string): string | undefined {
  if (!etag) return undefined;
  return etag.replace(/^"/, '').replace(/"$/, '');
}

function stripTrailingSlash(value: string): string {
  return value.replace(/\/$/, '');
}

/**
 * Best-effort detection of an S3 "object missing" error. S3-compatible services
 * answer a missing key with a 404 (`NoSuchKey`), surfaced by Bun as an `S3Error`.
 * Used to turn a missing object into `null` on `get`.
 */
function isNotFoundError(err: unknown): boolean {
  if (!err || typeof err !== 'object') return false;
  const candidate = err as { name?: string; code?: string; message?: string };
  if (candidate.code === 'NoSuchKey' || candidate.code === 'NoSuchBucket') return true;
  if (candidate.name !== 'S3Error') return false;
  const haystack = `${candidate.code ?? ''} ${candidate.message ?? ''}`.toLowerCase();
  return (
    haystack.includes('nosuchkey') ||
    haystack.includes('not found') ||
    haystack.includes('does not exist') ||
    haystack.includes('404')
  );
}
