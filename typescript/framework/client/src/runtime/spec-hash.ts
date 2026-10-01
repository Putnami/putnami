/**
 * Runtime-safe spec hashing.
 *
 * The generated client embeds a spec hash and the runtime drift check
 * recomputes it from the live service spec. This must run on every supported
 * runtime (Bun, Node, Deno, browsers), so it uses the portable Web Crypto API
 * (`crypto.subtle`) instead of Bun-only globals like `Bun.CryptoHasher`.
 *
 * The generator computes the *same* SHA-256 hash (build-time) so the embedded
 * and live hashes are comparable. Truncated to 16 hex chars to keep it short.
 */

/** Number of hex characters retained from the SHA-256 digest. */
const HASH_HEX_LENGTH = 16;

/**
 * Compute a truncated SHA-256 hash of content for spec drift detection.
 *
 * Async because Web Crypto's `digest` is async. The runtime drift check already
 * runs in an async context, so this adds no constraint there.
 */
export async function computeSpecHash(content: string): Promise<string> {
  const bytes = new TextEncoder().encode(content);
  const digest = await crypto.subtle.digest('SHA-256', bytes);
  return toHex(new Uint8Array(digest)).slice(0, HASH_HEX_LENGTH);
}

function toHex(bytes: Uint8Array): string {
  let hex = '';
  for (const byte of bytes) {
    hex += byte.toString(16).padStart(2, '0');
  }
  return hex;
}
