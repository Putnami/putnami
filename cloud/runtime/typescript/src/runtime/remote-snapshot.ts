import { syncFetch } from './sync-fetch';

/**
 * Durable last-known-good config snapshot loader.
 *
 * When the config server is unreachable for the whole retry budget, the
 * workload boots on a KMS-encrypted GCS snapshot the release path stamped for
 * this revision instead of failing the cold start. This is the TypeScript twin
 * of the Go snapshot loader and MUST match its encoding contract exactly:
 *
 *   GCS object text (base64 KMS ciphertext) → KMS Decrypt → base64 plaintext →
 *   JSON → resolved config tree.
 *
 * The object holds the base64 ciphertext string verbatim (exactly what Cloud KMS
 * Encrypt returns, stored as UTF-8 text), so it survives syncFetch's text
 * decoding and needs no binary handling. Everything here is synchronous:
 * ConfigSource.load() is sync, so the loader uses syncFetch for the metadata
 * access-token fetch, the GCS download, and the KMS Decrypt — no async, no
 * Promises, no @google-cloud/* SDK.
 */

/** GCS JSON API base for object media downloads. Overridable for tests. */
export const GCS_DOWNLOAD_BASE = 'https://storage.googleapis.com/storage/v1/b';
/** Cloud KMS v1 API base. Overridable for tests. */
export const KMS_API_BASE = 'https://cloudkms.googleapis.com/v1';
/** GCP metadata server access-token endpoint (cloud-platform scoped default SA). */
export const METADATA_ACCESS_TOKEN_URL =
  'http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token';

/**
 * Bounds the encrypted snapshot the loader reads into memory. A resolved config
 * tree is kilobytes; this ceiling defends against a corrupt or hostile object,
 * not a functional limit. It stays under Cloud KMS's 64 KiB Decrypt ciphertext
 * limit so an oversized object fails with a clear read error rather than an
 * opaque KMS 400. Mirrors snapshotObjectMaxBytes in the Go loader.
 */
const SNAPSHOT_OBJECT_MAX_BYTES = 256 * 1024;
const DEFAULT_SNAPSHOT_TIMEOUT_MS = 5000;
/** Serve a cached access token only while it has more than this many seconds of life left. */
const ACCESS_TOKEN_SKEW_SEC = 300;
/** Fallback lifetime when the metadata response omits a usable expires_in. */
const ACCESS_TOKEN_FALLBACK_TTL_SEC = 3600;

export interface GsUri {
  bucket: string;
  object: string;
}

/**
 * Splits a gs://bucket/object URI into its bucket and object parts. Rejects any
 * other scheme, a missing bucket, or a missing object so a misconfigured
 * CONFIG_SNAPSHOT_URI fails with a clear error rather than an opaque GCS 404.
 * Mirrors parseGSURI in the Go loader.
 */
export function parseGsUri(raw: string): GsUri {
  const trimmed = raw.trim();
  if (!trimmed) {
    throw new Error('config snapshot URI is empty');
  }
  let url: URL;
  try {
    url = new URL(trimmed);
  } catch (cause) {
    throw new Error(`parse config snapshot URI ${JSON.stringify(trimmed)}: ${errorText(cause)}`);
  }
  if (url.protocol !== 'gs:') {
    throw new Error(`config snapshot URI ${JSON.stringify(trimmed)} must use the gs:// scheme`);
  }
  const bucket = url.hostname;
  const object = url.pathname.replace(/^\/+/, '');
  if (!bucket || !object) {
    throw new Error(`config snapshot URI ${JSON.stringify(trimmed)} must be gs://<bucket>/<object>`);
  }
  return { bucket, object };
}

/**
 * Fetches the durable last-known-good config snapshot the release path stamped
 * for this workload and returns the decoded config tree. Throws on any failure
 * (bad URI, empty key, unreachable metadata/GCS/KMS, decode error); the caller
 * turns a throw into a graceful, non-fatal fallback miss.
 */
export function loadConfigSnapshot(
  snapshotUri: string,
  kmsKey: string,
  timeoutMs: number = DEFAULT_SNAPSHOT_TIMEOUT_MS,
): Record<string, unknown> {
  const { bucket, object } = parseGsUri(snapshotUri);
  if (!kmsKey.trim()) {
    throw new Error('config snapshot KMS key is empty');
  }

  const token = snapshotAccessToken(timeoutMs);
  const ciphertextB64 = downloadSnapshotObject(bucket, object, token, timeoutMs);
  const plaintextJson = decryptSnapshot(kmsKey, ciphertextB64, token, timeoutMs);

  let tree: unknown;
  try {
    tree = JSON.parse(plaintextJson);
  } catch (cause) {
    throw new Error(`decode config snapshot payload: ${errorText(cause)}`);
  }
  if (!tree || typeof tree !== 'object' || Array.isArray(tree)) {
    throw new Error('decode config snapshot payload: expected a JSON object');
  }
  return tree as Record<string, unknown>;
}

/**
 * Reads the encrypted snapshot object text from GCS with the workload's access
 * token. The response body IS the base64 ciphertext string. Mirrors
 * downloadSnapshotObject in the Go loader (including the size and empty guards).
 */
function downloadSnapshotObject(bucket: string, object: string, token: string, timeoutMs: number): string {
  const url = `${GCS_DOWNLOAD_BASE}/${bucket}/o/${encodeURIComponent(object)}?alt=media`;
  const response = syncFetch({
    url,
    method: 'GET',
    headers: { Authorization: `Bearer ${token}` },
    timeoutMs,
    throwOnHttpError: false,
  });
  if (response.status !== 200) {
    throw new Error(`download config snapshot gs://${bucket}/${object}: HTTP ${response.status}`);
  }
  const text = response.body;
  if (text.length > SNAPSHOT_OBJECT_MAX_BYTES) {
    throw new Error(`config snapshot gs://${bucket}/${object} exceeds ${SNAPSHOT_OBJECT_MAX_BYTES} bytes`);
  }
  if (text.trim().length === 0) {
    throw new Error(`config snapshot gs://${bucket}/${object} is empty`);
  }
  return text;
}

/**
 * Decrypts the base64 snapshot ciphertext with Cloud KMS and returns the UTF-8
 * plaintext (the resolved config tree JSON). The object already holds the base64
 * string KMS Encrypt produced, which is exactly what the REST Decrypt
 * `ciphertext` field expects, so it is passed through verbatim (trimmed); only
 * the base64 plaintext KMS returns is decoded. Mirrors decryptSnapshot in Go.
 */
function decryptSnapshot(kmsKey: string, ciphertextB64: string, token: string, timeoutMs: number): string {
  const response = syncFetch({
    url: `${KMS_API_BASE}/${kmsKey}:decrypt`,
    method: 'POST',
    headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' },
    body: JSON.stringify({ ciphertext: ciphertextB64.trim() }),
    timeoutMs,
    throwOnHttpError: false,
  });
  if (response.status !== 200) {
    throw new Error(`KMS decrypt config snapshot: HTTP ${response.status}`);
  }
  let parsed: { plaintext?: unknown };
  try {
    parsed = JSON.parse(response.body) as { plaintext?: unknown };
  } catch (cause) {
    throw new Error(`KMS decrypt config snapshot: decode response: ${errorText(cause)}`);
  }
  if (typeof parsed.plaintext !== 'string' || parsed.plaintext.length === 0) {
    throw new Error('KMS decrypt config snapshot: response missing plaintext');
  }
  return Buffer.from(parsed.plaintext, 'base64').toString('utf-8');
}

interface CachedAccessToken {
  value: string;
  expiresAtSec: number;
}

let cachedAccessToken: CachedAccessToken | undefined;

/**
 * Fetches a GCP OAuth access token (cloud-platform scope) for the default
 * service account from the metadata server. GCS and KMS need an ACCESS token,
 * not the ID token GcpMetadataTokenSource mints for the config server, so this
 * is a separate, small synchronous fetch. The token is cached in-process and
 * refreshed when it is within ACCESS_TOKEN_SKEW_SEC of expiry, matching the
 * caching style of GcpMetadataTokenSource. Off-GCP the metadata fetch fails,
 * which propagates as a thrown error so the snapshot fallback fails gracefully.
 */
function snapshotAccessToken(timeoutMs: number): string {
  const nowSec = Math.floor(Date.now() / 1000);
  if (cachedAccessToken && cachedAccessToken.expiresAtSec - nowSec > ACCESS_TOKEN_SKEW_SEC) {
    return cachedAccessToken.value;
  }
  const response = syncFetch({
    url: METADATA_ACCESS_TOKEN_URL,
    method: 'GET',
    headers: { 'Metadata-Flavor': 'Google' },
    timeoutMs,
    throwOnHttpError: false,
  });
  if (response.status !== 200) {
    throw new Error(`metadata access-token fetch returned HTTP ${response.status}`);
  }
  const parsed = JSON.parse(response.body) as { access_token?: unknown; expires_in?: unknown };
  const token = typeof parsed.access_token === 'string' ? parsed.access_token.trim() : '';
  if (!token) {
    throw new Error('metadata access-token response missing access_token');
  }
  const expiresIn =
    typeof parsed.expires_in === 'number' && parsed.expires_in > 0 ? parsed.expires_in : ACCESS_TOKEN_FALLBACK_TTL_SEC;
  cachedAccessToken = { value: token, expiresAtSec: nowSec + expiresIn };
  return token;
}

export function resetSnapshotAccessTokenCacheForTest(): void {
  cachedAccessToken = undefined;
}

function errorText(cause: unknown): string {
  return cause instanceof Error ? cause.message : String(cause);
}
