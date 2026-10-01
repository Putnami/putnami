import { createHmac, randomBytes, timingSafeEqual } from 'node:crypto';
import { dirname, join, resolve, sep } from 'node:path';
import { mkdir, readdir, rm } from 'node:fs/promises';
import { robustRemove, robustRename } from '@putnami/runtime/robustio';
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

// Re-exported for existing consumers (storage.server, backend barrel).
export { validateKey };

interface FileMetadataEntry {
  contentType?: string;
  custom?: Record<string, string>;
  cacheControl?: string;
  contentDisposition?: string;
  size: number;
  etag?: string;
  lastModified: string;
}

/**
 * StorageBackend implementation that stores objects on the local filesystem.
 * Used for local development. Metadata is stored alongside files as `.meta.json`.
 *
 * Directory layout:
 * ```
 * <dataDir>/
 *   <bucket>/
 *     <key>            # Object data
 *     <key>.meta.json  # Object metadata
 * ```
 */
export class FileBackend implements StorageBackend {
  /** HMAC secret used to sign local dev tokens. Generated per-process. */
  private readonly tokenSecret: string;

  /**
   * Per-key write lock. The object and its `.meta.json` sidecar are two separate
   * files committed by two renames, so without serialization two concurrent
   * writes to the same key could interleave their renames and leave the data
   * from one write paired with the metadata from another. Serializing writes per
   * destination key keeps each (data, metadata) pair atomic relative to other
   * writers. Reads are not locked: atomic rename already guarantees a reader
   * sees a whole file, and serialized writers guarantee a consistent pair.
   */
  private readonly writeLocks = new Map<string, Promise<unknown>>();

  constructor(
    private dataDir: string,
    tokenSecret?: string,
  ) {
    this.tokenSecret = tokenSecret ?? randomBytes(32).toString('hex');
  }

  /**
   * Run `fn` while holding the write lock for `${bucket}/${key}`. Calls for the
   * same key run strictly one after another; calls for different keys run
   * concurrently. The lock is released (and its map entry cleaned up) whether
   * `fn` resolves or rejects.
   */
  private async withWriteLock<R>(bucket: string, key: string, fn: () => Promise<R>): Promise<R> {
    const lockKey = `${bucket}/${key}`;
    const previous = this.writeLocks.get(lockKey) ?? Promise.resolve();
    // Chain onto the previous holder, swallowing its result/rejection so this
    // operation is not affected by an earlier operation's outcome.
    const run = previous.then(() => fn());
    // Store a settled-tracking promise so the next caller waits for us to finish.
    const settled = run.then(
      () => undefined,
      () => undefined,
    );
    this.writeLocks.set(lockKey, settled);
    try {
      return await run;
    } finally {
      // Only clear if no later operation has replaced us as the tail.
      if (this.writeLocks.get(lockKey) === settled) {
        this.writeLocks.delete(lockKey);
      }
    }
  }

  private async waitForWrite(bucket: string, key: string): Promise<void> {
    await this.writeLocks.get(`${bucket}/${key}`);
  }

  async put(
    bucket: string,
    key: string,
    data: Blob | Buffer | ReadableStream,
    metadata?: ObjectMetadata,
  ): Promise<PutResult> {
    return this.withWriteLock(bucket, key, () => this.putLocked(bucket, key, data, metadata));
  }

  private async putLocked(
    bucket: string,
    key: string,
    data: Blob | Buffer | ReadableStream,
    metadata?: ObjectMetadata,
  ): Promise<PutResult> {
    const filePath = this.objectPath(bucket, key);
    const metaPath = this.metaPath(bucket, key);
    await mkdir(this.dirPath(bucket, key), { recursive: true });

    // Write data and metadata to unique temp files first, then atomically rename
    // both into place. Two concurrent put()s to the same key each own private
    // temp files, so a reader never observes a half-written object or a
    // data/metadata pair from two different writes interleaved on disk.
    const dataTmp = tmpPath(filePath);
    const metaTmp = tmpPath(metaPath);

    let size: number;
    let etag: string;

    try {
      if (data instanceof Blob) {
        const bytes = new Uint8Array(await data.arrayBuffer());
        await Bun.write(dataTmp, bytes);
        size = bytes.length;
        etag = Bun.hash(bytes).toString(16);
      } else if (Buffer.isBuffer(data)) {
        const bytes = new Uint8Array(data);
        await Bun.write(dataTmp, bytes);
        size = bytes.length;
        etag = Bun.hash(bytes).toString(16);
      } else {
        // Stream directly to disk without buffering entire content in memory
        const writer = Bun.file(dataTmp).writer();
        const hasher = new Bun.CryptoHasher('sha256');
        size = 0;
        await data.pipeTo(
          new WritableStream({
            write(chunk) {
              writer.write(chunk);
              hasher.update(chunk);
              size += chunk.byteLength;
            },
            close() {
              writer.end();
            },
          }),
        );
        etag = hasher.digest('hex').substring(0, 16);
      }

      const metaEntry: FileMetadataEntry = {
        contentType: metadata?.contentType,
        custom: metadata?.custom,
        cacheControl: metadata?.cacheControl,
        contentDisposition: metadata?.contentDisposition,
        size,
        etag,
        lastModified: new Date().toISOString(),
      };
      await Bun.write(metaTmp, JSON.stringify(metaEntry));

      // Commit metadata first, then data. get() derives size from the data file
      // and reads contentType/etag from metadata, so renaming data last means a
      // reader that observes the new object also observes its new metadata.
      // On Windows a rename over a file that a reader holds open fails until
      // the reader closes it; robustRename waits that out.
      await robustRename(metaTmp, metaPath);
      await robustRename(dataTmp, filePath);
    } catch (error) {
      await Promise.all([rm(dataTmp, { force: true }), rm(metaTmp, { force: true })]);
      throw error;
    }

    return { key, size, etag };
  }

  async get(bucket: string, key: string): Promise<GetResult | null> {
    await this.waitForWrite(bucket, key);

    const filePath = this.objectPath(bucket, key);
    const file = Bun.file(filePath);

    if (!(await file.exists())) return null;

    const meta = await this.readMeta(bucket, key);

    return {
      key,
      body: file.stream(),
      size: file.size,
      contentType: meta?.contentType,
      etag: meta?.etag,
      lastModified: meta?.lastModified ? new Date(meta.lastModified) : undefined,
      metadata: meta?.custom,
    };
  }

  async delete(bucket: string, key: string): Promise<void> {
    await this.withWriteLock(bucket, key, async () => {
      const filePath = this.objectPath(bucket, key);
      const metaFilePath = this.metaPath(bucket, key);
      await robustRemove(filePath);
      await robustRemove(metaFilePath);
    });
  }

  async list(bucket: string, options?: ListOptions): Promise<ListResult> {
    const bucketDir = join(this.dataDir, bucket);
    const allKeys = await this.listKeysRecursive(bucketDir, '');

    const prefix = options?.prefix ?? '';
    const delimiter = options?.delimiter;
    const maxKeys = options?.maxKeys ?? 1000;

    // Exclude metadata sidecars and in-flight atomic-write temp files so a
    // concurrent put()'s transient `.tmp` never surfaces as a listed object.
    const filtered = allKeys.filter((k) => k.startsWith(prefix) && !k.endsWith('.meta.json') && !k.endsWith('.tmp'));

    if (!delimiter) {
      const objects = filtered.slice(0, maxKeys).map((key) => this.statObject(bucket, key));
      return {
        objects,
        prefixes: [],
        isTruncated: filtered.length > maxKeys,
        continuationToken: filtered.length > maxKeys ? String(maxKeys) : undefined,
      };
    }

    // Group by delimiter
    const prefixSet = new Set<string>();
    const directObjects: string[] = [];

    for (const key of filtered) {
      const rest = key.slice(prefix.length);
      const delimIdx = rest.indexOf(delimiter);
      if (delimIdx >= 0) {
        prefixSet.add(prefix + rest.slice(0, delimIdx + delimiter.length));
      } else {
        directObjects.push(key);
      }
    }

    const objects = directObjects.slice(0, maxKeys).map((key) => this.statObject(bucket, key));

    return {
      objects,
      prefixes: [...prefixSet].sort(),
      isTruncated: false,
    };
  }

  async exists(bucket: string, key: string): Promise<boolean> {
    return Bun.file(this.objectPath(bucket, key)).exists();
  }

  async copy(bucket: string, source: string, destination: string): Promise<void> {
    // Lock the destination key: copy writes the destination's data + metadata
    // pair and must not interleave with a concurrent put()/copy() to the same key.
    return this.withWriteLock(bucket, destination, () => this.copyLocked(bucket, source, destination));
  }

  private async copyLocked(bucket: string, source: string, destination: string): Promise<void> {
    const srcFile = Bun.file(this.objectPath(bucket, source));
    if (!(await srcFile.exists())) {
      throw new StorageError(`Source object not found: ${bucket}/${source}`, 'NOT_FOUND');
    }

    const destPath = this.objectPath(bucket, destination);
    const destMetaPath = this.metaPath(bucket, destination);
    await mkdir(this.dirPath(bucket, destination), { recursive: true });

    // Stage to temp files and rename so a reader of the destination never sees a
    // partially-copied object or a stale data/metadata pairing.
    const dataTmp = tmpPath(destPath);
    const metaTmp = tmpPath(destMetaPath);
    const meta = await this.readMeta(bucket, source);

    try {
      await Bun.write(dataTmp, srcFile);
      if (meta) {
        meta.lastModified = new Date().toISOString();
        await Bun.write(metaTmp, JSON.stringify(meta));
      }
      // Commit metadata first, then data (see put()): renaming data last means a
      // reader that sees the copied object also sees its copied metadata.
      if (meta) {
        await robustRename(metaTmp, destMetaPath);
      }
      await robustRename(dataTmp, destPath);
    } catch (error) {
      await Promise.all([rm(dataTmp, { force: true }), rm(metaTmp, { force: true })]);
      throw error;
    }
  }

  url(bucket: string, key: string): string {
    return this.objectPath(bucket, key);
  }

  async signedUploadUrl(bucket: string, key: string, options?: SignOptions): Promise<SignedUrl> {
    validateKey(key);
    // For file backend, signed URLs point to the local storage server
    const expiresAt = new Date(Date.now() + resolveExpiry(options?.expiresIn));
    const token = generateToken(this.tokenSecret, 'PUT', bucket, key, expiresAt);

    return {
      url: `/_storage/${encodeURIComponent(bucket)}/${encodeKeyPath(key)}?token=${token}`,
      expiresAt,
      method: 'PUT',
    };
  }

  async signedDownloadUrl(bucket: string, key: string, options?: SignOptions): Promise<SignedUrl> {
    validateKey(key);
    const expiresAt = new Date(Date.now() + resolveExpiry(options?.expiresIn));
    const token = generateToken(this.tokenSecret, 'GET', bucket, key, expiresAt);

    return {
      url: `/_storage/${encodeURIComponent(bucket)}/${encodeKeyPath(key)}?token=${token}`,
      expiresAt,
      method: 'GET',
    };
  }

  /**
   * Validate a signed URL token. Used by the local storage server.
   *
   * `method` is the HTTP method of the incoming request; the token is only
   * valid if it was signed for that same method, so a download (GET) URL can
   * never authorize an upload (PUT) and vice-versa.
   */
  validateToken(method: string, token: string, bucket: string, key: string): boolean {
    return validateToken(this.tokenSecret, method, token, bucket, key);
  }

  async close(): Promise<void> {
    // Nothing to close for filesystem
  }

  // ---- Internal Helpers ----

  private objectPath(bucket: string, key: string): string {
    return safePath(this.dataDir, bucket, key);
  }

  private metaPath(bucket: string, key: string): string {
    return `${safePath(this.dataDir, bucket, key)}.meta.json`;
  }

  private dirPath(bucket: string, key: string): string {
    return dirname(safePath(this.dataDir, bucket, key));
  }

  /**
   * Return ObjectInfo using filesystem stat (size, lastModified) without reading .meta.json.
   * Used by list() to avoid N individual metadata file reads.
   */
  private statObject(bucket: string, key: string): { key: string; size: number; lastModified: Date } {
    const file = Bun.file(this.objectPath(bucket, key));
    return { key, size: file.size, lastModified: new Date(file.lastModified) };
  }

  private async readMeta(bucket: string, key: string): Promise<FileMetadataEntry | null> {
    const metaFile = Bun.file(this.metaPath(bucket, key));
    if (!(await metaFile.exists())) return null;
    try {
      return JSON.parse(await metaFile.text()) as FileMetadataEntry;
    } catch {
      return null;
    }
  }

  private async listKeysRecursive(dir: string, prefix: string): Promise<string[]> {
    const keys: string[] = [];
    try {
      const entries = await readdir(dir, { withFileTypes: true });
      for (const entry of entries) {
        const entryPath = prefix ? `${prefix}/${entry.name}` : entry.name;
        if (entry.isDirectory()) {
          keys.push(...(await this.listKeysRecursive(join(dir, entry.name), entryPath)));
        } else {
          keys.push(entryPath);
        }
      }
    } catch {
      // Directory doesn't exist yet
    }
    return keys;
  }
}

// ---- Path Safety ----

/**
 * Build a unique sibling temp path for an atomic write. Each call yields a
 * distinct name (pid + uuid) so concurrent writers never share a temp file or
 * race on rename(). The `.tmp` suffix is filtered out of list() via the
 * `.meta.json` exclusion only for metadata, so temp paths must never collide
 * with real keys — the random suffix guarantees that.
 */
function tmpPath(target: string): string {
  return `${target}.${process.pid}.${crypto.randomUUID()}.tmp`;
}

/** The path operations safePath needs; tests pass `path.win32`. */
export type PathApi = { resolve: (...paths: string[]) => string; sep: string };

/**
 * Build a safe filesystem path from dataDir + bucket + key.
 * Validates that the resolved path stays within the data directory,
 * with the host's separator.
 */
export function safePath(dataDir: string, bucket: string, key: string, path: PathApi = { resolve, sep }): string {
  validateKey(key);
  const base = path.resolve(dataDir, bucket);
  const full = path.resolve(base, key);
  if (!full.startsWith(`${base}${path.sep}`) && full !== base) {
    throw new StorageError(`Path escapes bucket root: "${key}"`, 'PATH_TRAVERSAL');
  }
  return full;
}

// ---- Duration Parsing ----

const DURATION_UNITS: Record<string, number> = {
  s: 1000,
  m: 60 * 1000,
  h: 60 * 60 * 1000,
  d: 24 * 60 * 60 * 1000,
};

export function parseDuration(duration: string): number {
  const match = duration.match(/^(\d+)(s|m|h|d)$/);
  if (!match) throw new Error(`Invalid duration format: "${duration}". Expected format like "15m", "1h".`);
  return Number.parseInt(match[1], 10) * DURATION_UNITS[match[2]];
}

/** Default signed-URL validity when no `expiresIn` is supplied. */
export const DEFAULT_SIGNED_URL_EXPIRY = '15m';

/**
 * Maximum signed-URL validity. Signed URLs are bearer credentials; an unbounded
 * expiry turns a leaked URL into a long-lived (effectively permanent) grant.
 * Requests above this cap are rejected at signing time.
 */
export const MAX_SIGNED_URL_DURATION_MS = parseDuration('7d');

/**
 * Resolve and validate the signed-URL expiry (in ms) for the given `expiresIn`.
 * Falls back to {@link DEFAULT_SIGNED_URL_EXPIRY} when omitted and rejects any
 * value above {@link MAX_SIGNED_URL_DURATION_MS}.
 */
export function resolveExpiry(expiresIn?: string): number {
  const ms = parseDuration(expiresIn ?? DEFAULT_SIGNED_URL_EXPIRY);
  if (ms > MAX_SIGNED_URL_DURATION_MS) {
    throw new StorageError(
      `Signed URL expiry "${expiresIn}" exceeds the maximum of 7d. ` +
        'Use a shorter expiresIn; long-lived signed URLs are a credential-leak risk.',
      'EXPIRY_TOO_LONG',
    );
  }
  return ms;
}

// ---- HMAC Token Signing ----

/**
 * Generate an HMAC-signed token for a signed URL.
 * The token contains the payload and its HMAC-SHA256 signature.
 *
 * The HTTP method is part of the signed payload so a token minted for one
 * method (e.g. GET/download) can never authorize another (e.g. PUT/upload).
 */
function generateToken(secret: string, method: string, bucket: string, key: string, expiresAt: Date): string {
  const payload = `${method.toUpperCase()}:${bucket}:${key}:${expiresAt.getTime()}`;
  const signature = createHmac('sha256', secret).update(payload).digest('base64url');
  return `${Buffer.from(payload).toString('base64url')}.${signature}`;
}

/**
 * Validate an HMAC-signed token for a signed URL.
 * Checks signature integrity, method/bucket/key match, and expiration.
 */
export function validateToken(secret: string, method: string, token: string, bucket: string, key: string): boolean {
  try {
    const dotIndex = token.indexOf('.');
    if (dotIndex < 0) return false;

    const payloadB64 = token.slice(0, dotIndex);
    const signatureB64 = token.slice(dotIndex + 1);

    const payload = Buffer.from(payloadB64, 'base64url').toString();
    const expectedSignature = createHmac('sha256', secret).update(payload).digest('base64url');

    // Constant-time comparison to prevent timing attacks
    const sigBuf = Buffer.from(signatureB64, 'base64url');
    const expectedBuf = Buffer.from(expectedSignature, 'base64url');
    if (sigBuf.length !== expectedBuf.length) return false;
    if (!timingSafeEqual(sigBuf, expectedBuf)) return false;

    const parts = payload.split(':');
    // key may contain colons, so rejoin everything after method+bucket and before the last part (timestamp)
    const tokenMethod = parts[0];
    const tokenBucket = parts[1];
    const tokenExpiry = parts[parts.length - 1];
    const tokenKey = parts.slice(2, -1).join(':');
    const expiresAt = Number.parseInt(tokenExpiry, 10);

    return tokenMethod === method.toUpperCase() && tokenBucket === bucket && tokenKey === key && Date.now() < expiresAt;
  } catch {
    return false;
  }
}
