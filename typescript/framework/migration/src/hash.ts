import { createHash } from 'node:crypto';

/**
 * SHA-256 hex digest of a string. Uses Bun's CryptoHasher when
 * available; falls back to the Web Crypto API for non-Bun runtimes.
 * The output is byte-for-byte identical to the Go runner's
 * sha256Hex helper, which keeps the state-store hash column
 * consistent across language runtimes.
 */
export async function sha256Hex(content: string): Promise<string> {
  // Bun fast path
  const bun = (
    globalThis as {
      Bun?: { CryptoHasher: new (alg: string) => { update(s: string): void; digest(enc: string): string } };
    }
  ).Bun;
  if (bun?.CryptoHasher) {
    const hasher = new bun.CryptoHasher('sha256');
    hasher.update(content);
    return hasher.digest('hex');
  }
  // Web Crypto fallback (Node 20+, browsers)
  const bytes = new TextEncoder().encode(content);
  const buf = await crypto.subtle.digest('SHA-256', bytes);
  return [...new Uint8Array(buf)].map((b) => b.toString(16).padStart(2, '0')).join('');
}

/**
 * Synchronous SHA-256 hex digest for places that can't await. Uses Bun's
 * CryptoHasher when available; otherwise falls back to Node's
 * `node:crypto` `createHash` (Node, and any runtime providing the builtin).
 * The output matches {@link sha256Hex} and the Go runner byte-for-byte.
 *
 * Note: there is no synchronous Web Crypto API, so a runtime that provides
 * neither Bun nor `node:crypto` (e.g. a bare browser) cannot use this helper —
 * await {@link sha256Hex} there instead.
 */
export function sha256HexSync(content: string): string {
  const bun = (
    globalThis as {
      Bun?: { CryptoHasher: new (alg: string) => { update(s: string): void; digest(enc: string): string } };
    }
  ).Bun;
  if (bun?.CryptoHasher) {
    const hasher = new bun.CryptoHasher('sha256');
    hasher.update(content);
    return hasher.digest('hex');
  }
  // Node fallback (and any runtime exposing node:crypto).
  return createHash('sha256').update(content, 'utf8').digest('hex');
}
