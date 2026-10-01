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
import type { StorageBackend } from './storage.backend';
import { encodeKeyPath, validateKey } from './key.utils';

/** Default timeout for HTTP requests to the remote storage service (30 seconds). */
const DEFAULT_TIMEOUT_MS = 30_000;

export interface RemoteBackendOptions {
  /** Base URL of the storage service (e.g., 'https://storage.putnami.cloud') */
  endpoint: string;
  /**
   * Access key sent as `Authorization: Bearer <accessKey>` on every request.
   * This is the only credential the remote backend uses: the storage service
   * authenticates the bearer token and mints signed URLs server-side (via the
   * `_sign/*` endpoints), so no client-side request signing is performed.
   */
  accessKey?: string;
  /** Timeout in milliseconds for each HTTP request. Defaults to 30 000 ms. */
  timeoutMs?: number;
}

/**
 * StorageBackend implementation that communicates with storage.putnami.cloud
 * (or any compatible remote storage API) over HTTP.
 *
 * Authentication is a single bearer token (`accessKey`); the service performs
 * any required signing server-side. There is no client-side secret-key signing.
 */
export class RemoteBackend implements StorageBackend {
  private endpoint: string;
  private accessKey?: string;
  private timeoutMs: number;

  constructor(options: RemoteBackendOptions) {
    // Strip trailing slash
    this.endpoint = options.endpoint.replace(/\/$/, '');
    this.accessKey = options.accessKey;
    this.timeoutMs = options.timeoutMs ?? DEFAULT_TIMEOUT_MS;
  }

  async put(
    bucket: string,
    key: string,
    data: Blob | Buffer | ReadableStream,
    metadata?: ObjectMetadata,
  ): Promise<PutResult> {
    const headers = this.buildHeaders(metadata);
    const body: BodyInit = Buffer.isBuffer(data) ? new Uint8Array(data) : data;

    const response = await fetch(this.objectUrl(bucket, key), {
      method: 'PUT',
      headers,
      body,
      signal: AbortSignal.timeout(this.timeoutMs),
    });

    if (!response.ok) {
      throw new StorageRemoteError(`PUT ${bucket}/${key}`, response.status, await response.text());
    }

    const result = await response.json();
    return result as PutResult;
  }

  async get(bucket: string, key: string): Promise<GetResult | null> {
    const response = await fetch(this.objectUrl(bucket, key), {
      method: 'GET',
      headers: this.authHeaders(),
      signal: AbortSignal.timeout(this.timeoutMs),
    });

    if (response.status === 404) return null;

    if (!response.ok) {
      throw new StorageRemoteError(`GET ${bucket}/${key}`, response.status, await response.text());
    }

    return {
      key,
      body: response.body!,
      size: Number(response.headers.get('content-length') ?? 0),
      contentType: response.headers.get('content-type') ?? undefined,
      etag: response.headers.get('etag') ?? undefined,
      lastModified: response.headers.has('last-modified')
        ? new Date(response.headers.get('last-modified')!)
        : undefined,
      metadata: parseCustomMetadata(response.headers),
    };
  }

  async delete(bucket: string, key: string): Promise<void> {
    const response = await fetch(this.objectUrl(bucket, key), {
      method: 'DELETE',
      headers: this.authHeaders(),
      signal: AbortSignal.timeout(this.timeoutMs),
    });

    if (!response.ok && response.status !== 404) {
      throw new StorageRemoteError(`DELETE ${bucket}/${key}`, response.status, await response.text());
    }
  }

  async list(bucket: string, options?: ListOptions): Promise<ListResult> {
    const params = new URLSearchParams();
    if (options?.prefix) params.set('prefix', options.prefix);
    if (options?.delimiter) params.set('delimiter', options.delimiter);
    if (options?.maxKeys) params.set('max-keys', String(options.maxKeys));
    if (options?.continuationToken) params.set('continuation-token', options.continuationToken);

    const query = params.toString();
    const url = `${this.endpoint}/${encodeURIComponent(bucket)}${query ? `?${query}` : ''}`;

    const response = await fetch(url, {
      method: 'GET',
      headers: this.authHeaders(),
      signal: AbortSignal.timeout(this.timeoutMs),
    });

    if (!response.ok) {
      throw new StorageRemoteError(`LIST ${bucket}`, response.status, await response.text());
    }

    return reviveListResult(await response.json());
  }

  async exists(bucket: string, key: string): Promise<boolean> {
    const response = await fetch(this.objectUrl(bucket, key), {
      method: 'HEAD',
      headers: this.authHeaders(),
      signal: AbortSignal.timeout(this.timeoutMs),
    });
    return response.ok;
  }

  async copy(bucket: string, source: string, destination: string): Promise<void> {
    validateKey(source);
    const response = await fetch(this.objectUrl(bucket, destination), {
      method: 'PUT',
      headers: {
        ...this.authHeaders(),
        'x-copy-source': `${encodeURIComponent(bucket)}/${encodeKeyPath(source)}`,
      },
      signal: AbortSignal.timeout(this.timeoutMs),
    });

    if (!response.ok) {
      const body = await response.text();
      // The destination is being written, so the only object a copy can be
      // missing is the source. Surface a 404 as the same typed NOT_FOUND error
      // the in-process backends throw, so callers can branch on it uniformly
      // regardless of which backend is configured.
      if (response.status === 404) {
        throw new StorageError(
          `Source object not found: ${bucket}/${source}`,
          'NOT_FOUND',
          new StorageRemoteError(`COPY ${bucket}/${source} -> ${destination}`, response.status, body),
        );
      }
      throw new StorageRemoteError(`COPY ${bucket}/${source} -> ${destination}`, response.status, body);
    }
  }

  url(bucket: string, key: string): string {
    return this.objectUrl(bucket, key);
  }

  async signedUploadUrl(bucket: string, key: string, options?: SignOptions): Promise<SignedUrl> {
    validateKey(key);
    const response = await fetch(`${this.endpoint}/${encodeURIComponent(bucket)}/_sign/upload`, {
      method: 'POST',
      headers: {
        ...this.authHeaders(),
        'content-type': 'application/json',
      },
      body: JSON.stringify({ key, ...options }),
      signal: AbortSignal.timeout(this.timeoutMs),
    });

    if (!response.ok) {
      throw new StorageRemoteError(`SIGN UPLOAD ${bucket}/${key}`, response.status, await response.text());
    }

    const result = await response.json();
    return { ...result, expiresAt: new Date(result.expiresAt), method: 'PUT' } as SignedUrl;
  }

  async signedDownloadUrl(bucket: string, key: string, options?: SignOptions): Promise<SignedUrl> {
    validateKey(key);
    const response = await fetch(`${this.endpoint}/${encodeURIComponent(bucket)}/_sign/download`, {
      method: 'POST',
      headers: {
        ...this.authHeaders(),
        'content-type': 'application/json',
      },
      body: JSON.stringify({ key, ...options }),
      signal: AbortSignal.timeout(this.timeoutMs),
    });

    if (!response.ok) {
      throw new StorageRemoteError(`SIGN DOWNLOAD ${bucket}/${key}`, response.status, await response.text());
    }

    const result = await response.json();
    return { ...result, expiresAt: new Date(result.expiresAt), method: 'GET' } as SignedUrl;
  }

  async close(): Promise<void> {
    // HTTP client has no persistent resources to close
  }

  // ---- Internal Helpers ----

  private authHeaders(): Record<string, string> {
    const headers: Record<string, string> = {};
    if (this.accessKey) {
      headers['authorization'] = `Bearer ${this.accessKey}`;
    }
    return headers;
  }

  private buildHeaders(metadata?: ObjectMetadata): Record<string, string> {
    const headers: Record<string, string> = this.authHeaders();
    if (metadata?.contentType) headers['content-type'] = metadata.contentType;
    if (metadata?.cacheControl) headers['cache-control'] = metadata.cacheControl;
    if (metadata?.contentDisposition) headers['content-disposition'] = metadata.contentDisposition;
    if (metadata?.custom) {
      for (const [k, v] of Object.entries(metadata.custom)) {
        headers[`x-meta-${k}`] = v;
      }
    }
    return headers;
  }

  /**
   * Builds the object request URL, validating the key for path traversal and
   * percent-encoding each path segment of the bucket and key so a key cannot
   * inject query strings, fragments, or extra path separators into the request.
   */
  private objectUrl(bucket: string, key: string): string {
    validateKey(key);
    return `${this.endpoint}/${encodeURIComponent(bucket)}/${encodeKeyPath(key)}`;
  }
}

// ---- Helpers ----

/**
 * Rebuild a {@link ListResult} from a JSON payload.
 *
 * `ObjectInfo.lastModified` is a `Date` in the backend contract, and every
 * in-process backend (memory, file, S3) returns one. JSON has no date type, so
 * the wire carries a string: handing the parsed payload straight back would
 * make `lastModified` a `string` at runtime while its declared type says
 * `Date`, and only when the remote backend is configured. Revive it here so the
 * same call returns the same shape on every backend. A value the server omits
 * stays absent, and an unparseable one is dropped rather than surfaced as an
 * `Invalid Date`.
 */
function reviveListResult(payload: unknown): ListResult {
  const result = payload as ListResult;
  const objects = Array.isArray(result?.objects) ? result.objects : [];
  return {
    ...result,
    objects: objects.map((object) => {
      const lastModified = reviveDate(object.lastModified);
      if (lastModified === undefined) {
        const { lastModified: _dropped, ...rest } = object;
        return rest;
      }
      return { ...object, lastModified };
    }),
    prefixes: Array.isArray(result?.prefixes) ? result.prefixes : [],
    isTruncated: result?.isTruncated === true,
  };
}

function reviveDate(value: unknown): Date | undefined {
  if (value === undefined || value === null) return undefined;
  const date = value instanceof Date ? value : new Date(value as string | number);
  return Number.isNaN(date.getTime()) ? undefined : date;
}

function parseCustomMetadata(headers: Headers): Record<string, string> | undefined {
  const meta: Record<string, string> = {};
  headers.forEach((value, key) => {
    if (key.startsWith('x-meta-')) {
      meta[key.slice(7)] = value;
    }
  });
  return Object.keys(meta).length > 0 ? meta : undefined;
}

// ---- Errors ----

/** Maximum byte length for stored response bodies. Larger payloads are truncated. */
const MAX_RESPONSE_BODY_LENGTH = 4096;

export class StorageRemoteError extends Error {
  readonly operation: string;
  readonly statusCode: number;
  /** Response body from the server. Non-enumerable to prevent accidental serialization of sensitive data. */
  declare readonly responseBody: string;

  constructor(operation: string, statusCode: number, body: string) {
    super(`Storage remote error: ${operation} returned ${statusCode}`);
    this.name = 'StorageRemoteError';
    this.operation = operation;
    this.statusCode = statusCode;
    // Non-enumerable: won't appear in JSON.stringify or logger serializers,
    // preventing accidental leakage of server internals (SQL errors, stack traces, PII).
    Object.defineProperty(this, 'responseBody', {
      value: body.length > MAX_RESPONSE_BODY_LENGTH ? `${body.slice(0, MAX_RESPONSE_BODY_LENGTH)}… [truncated]` : body,
      enumerable: false,
      writable: false,
    });
  }
}
