import { timingSafeEqualBytes } from './security.utils';

// Versioned secret digests: the TypeScript twin of go/framework/security
// (secret.go). A digest produced by hashSecret is exactly the canonical keyring
// PHC wire form (e.g. $pbkdf2-sha256$v=1$i=600000$<salt>$<hash>); it parses
// clean through parseDigest and round-trips byte-for-byte with the Go
// implementation. The on-disk string is the single cross-language contract, so
// the grammar constants, the base64 alphabet (RFC 4648 §4 STANDARD, NO padding —
// matching Go's base64.RawStdEncoding), the UTF-8 secret encoding, and the HMAC
// key/message order (key = salt, message = secret) must all stay identical to
// the Go source. The shared fixture protocols/keyring/fixtures/secret-vectors.json
// pins the exact bytes both languages must reproduce. No secret or key material
// is ever logged from this module.
//
// This re-implements (does NOT import) the keyring PHC grammar because there is
// no TypeScript keyring package. Keep these constants in sync with
// go.putnami.dev/protocol/keyring; the shared fixture is what makes a drift
// between the two a test failure rather than a runtime mismatch.

const TEXT_ENCODER = new TextEncoder();

// --- Grammar constants (mirror go.putnami.dev/protocol/keyring) --------------

/** The `v=1` segment of the PHC grammar (keyring.DigestVersion). */
const DIGEST_VERSION = 1;

/**
 * Grammar bounds on the pbkdf2 iteration count (keyring.MinIterations /
 * keyring.MaxIterations). These merely bound a well-formed string; the security
 * floor a caller cannot go below is {@link ITERATION_FLOOR}.
 */
const MIN_ITERATIONS = 1;
const MAX_ITERATIONS = 10_000_000;

// --- Security policy constants (mirror go/framework/security secret.go) -------

/**
 * Current pbkdf2-sha256 work-factor preset (OWASP 2023 guidance). A stored
 * digest below this triggers a rehash on the next successful verify.
 */
export const DEFAULT_PBKDF2_ITERATIONS = 600_000;

/**
 * Security floor for the tunable pbkdf2 work factor: {@link HashOptions.iterations}
 * is clamped up to this and can never configure a value below it, so a mis-tuned
 * deployment can never fall to an unsafe work factor. It sits well above the
 * grammar bound {@link MIN_ITERATIONS}.
 */
export const ITERATION_FLOOR = 210_000;

/** Random salt length in bytes (128 bits). */
const DEFAULT_SALT_LENGTH = 16;

/** Derived-key / HMAC output length in bytes (one SHA-256 block). */
const HASH_LENGTH = 32;

// --- Types -------------------------------------------------------------------

/**
 * A PHC-style credential-digest algorithm (keyring.DigestAlgorithm). The set is
 * intentionally closed.
 */
export type DigestAlgorithm = 'pbkdf2-sha256' | 'hmac-sha256';

/**
 * A parsed PHC-style credential digest. `salt` and `hash` are the RAW decoded
 * bytes; the wire form base64-encodes them with standard RFC 4648 §4 base64
 * WITHOUT padding. `iterations` is meaningful only for pbkdf2-sha256 (0 for
 * hmac-sha256).
 */
export interface Digest {
  algorithm: DigestAlgorithm;
  version: number;
  iterations: number;
  // Backed by a plain ArrayBuffer so the raw bytes can flow straight into
  // WebCrypto (whose BufferSource inputs reject Uint8Array<ArrayBufferLike>).
  salt: Uint8Array<ArrayBuffer>;
  hash: Uint8Array<ArrayBuffer>;
}

/** Options for {@link hashSecret}. Applied over the defaults. */
export interface HashOptions {
  /**
   * Digest algorithm. `pbkdf2-sha256` (default) for low-entropy secrets such as
   * passwords; `hmac-sha256` for high-entropy secrets such as randomly generated
   * API tokens, where a work factor buys nothing.
   */
  algorithm?: DigestAlgorithm;
  /**
   * pbkdf2-sha256 work factor. CLAMPED into [{@link ITERATION_FLOOR},
   * {@link MAX_ITERATIONS}]: a caller can raise the cost but never drop below the
   * floor. Ignored for hmac-sha256 (which carries no work factor).
   */
  iterations?: number;
}

/**
 * Outcome of {@link verifySecret}. `valid` reports whether the secret matched
 * (compared in constant time). `rehashRequired` is meaningful only when `valid`
 * is true; a failed verify always reports `false`. Mirror of Go's VerifyResult.
 */
export interface VerifyResult {
  valid: boolean;
  rehashRequired: boolean;
}

// --- Base64 (RFC 4648 §4 STANDARD, no padding — matches base64.RawStdEncoding) --

const STD_ALPHABET = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/';
const STD_LOOKUP: Record<string, number> = {};
for (let i = 0; i < STD_ALPHABET.length; i++) STD_LOOKUP[STD_ALPHABET[i]] = i;

/** Encodes bytes as unpadded standard base64 (RFC 4648 §4). */
function encodeBase64RawStd(bytes: Uint8Array): string {
  let bin = '';
  for (let i = 0; i < bytes.length; i++) bin += String.fromCharCode(bytes[i]);
  return btoa(bin).replace(/=+$/, '');
}

/**
 * Decodes unpadded standard base64 (RFC 4648 §4), rejecting any character
 * outside the standard alphabet (padding, base64url `-`/`_` and whitespace all
 * rejected) and an impossible trailing quantum of a single character. Returns
 * `undefined` on malformed input so callers fail closed — mirror of
 * base64.RawStdEncoding.DecodeString.
 */
function decodeBase64RawStd(s: string): Uint8Array<ArrayBuffer> | undefined {
  if (s.length % 4 === 1) return undefined;
  const out: number[] = [];
  let buffer = 0;
  let bits = 0;
  for (let i = 0; i < s.length; i++) {
    const v = STD_LOOKUP[s[i]];
    if (v === undefined) return undefined;
    buffer = ((buffer << 6) | v) & 0xff_ff_ff;
    bits += 6;
    if (bits >= 8) {
      bits -= 8;
      out.push((buffer >> bits) & 0xff);
    }
  }
  return new Uint8Array(out);
}

// --- PHC digest grammar (mirror keyring ParseDigest / Digest.String) ---------

/**
 * Parses a keyed `<key>=<int>` segment, returning the integer or `undefined`
 * when the segment is malformed. Rejects a leading `+`/`-` and any non-decimal
 * character so only a canonical unsigned decimal parses (mirror of
 * keyring.parseKeyedInt).
 */
function parseKeyedInt(seg: string, key: string): number | undefined {
  const prefix = `${key}=`;
  if (!seg.startsWith(prefix)) return undefined;
  const rest = seg.slice(prefix.length);
  if (rest.length === 0 || !/^[0-9]+$/.test(rest)) return undefined;
  const n = Number.parseInt(rest, 10);
  if (!Number.isSafeInteger(n)) return undefined;
  return n;
}

/**
 * Strictly parses and validates a PHC-style credential digest string into a
 * {@link Digest}. Rejects a malformed structure, an unknown algorithm, an
 * unsupported version, out-of-range or malformed parameters, non-canonical
 * base64, and an empty salt/hash, returning `undefined` in every failure case so
 * callers fail closed. Equivalent to keyring.ParseAndValidateDigest.
 *
 * Grammar:
 *
 *   $pbkdf2-sha256$v=1$i=<iterations>$<b64-salt>$<b64-hash>
 *   $hmac-sha256$v=1$<b64-salt>$<b64-hash>
 */
export function parseDigest(s: string): Digest | undefined {
  if (s.length === 0 || s[0] !== '$') return undefined;
  // A leading '$' yields an empty first segment; a trailing '$' or empty interior
  // segment is caught by the exact segment-count checks below.
  const segs = s.split('$');
  if (segs.length < 4) return undefined;

  const algorithm = segs[1];
  if (algorithm !== 'pbkdf2-sha256' && algorithm !== 'hmac-sha256') return undefined;

  const version = parseKeyedInt(segs[2], 'v');
  if (version === undefined || version !== DIGEST_VERSION) return undefined;

  if (algorithm === 'pbkdf2-sha256') {
    // $pbkdf2-sha256$v=1$i=<n>$<salt>$<hash> → 6 segments incl. leading empty.
    if (segs.length !== 6) return undefined;
    const iterations = parseKeyedInt(segs[3], 'i');
    if (iterations === undefined || iterations < MIN_ITERATIONS || iterations > MAX_ITERATIONS) return undefined;
    const salt = decodeBase64RawStd(segs[4]);
    const hash = decodeBase64RawStd(segs[5]);
    if (!salt || salt.length === 0 || !hash || hash.length === 0) return undefined;
    return { algorithm, version, iterations, salt, hash };
  }

  // $hmac-sha256$v=1$<salt>$<hash> → 5 segments incl. leading empty.
  if (segs.length !== 5) return undefined;
  const salt = decodeBase64RawStd(segs[3]);
  const hash = decodeBase64RawStd(segs[4]);
  if (!salt || salt.length === 0 || !hash || hash.length === 0) return undefined;
  return { algorithm, version, iterations: 0, salt, hash };
}

/**
 * Validates a programmatically-constructed {@link Digest} before encoding,
 * returning a list of human-readable problems (empty when valid). Mirror of
 * keyring.ValidateDigest — the guard that keeps a malformed digest from ever
 * being emitted.
 */
function validateDigest(d: Digest): string[] {
  const errs: string[] = [];
  if (d.algorithm !== 'pbkdf2-sha256' && d.algorithm !== 'hmac-sha256') {
    errs.push(`unknown digest algorithm ${JSON.stringify(d.algorithm)}`);
  }
  if (d.version !== DIGEST_VERSION) errs.push(`unsupported digest version ${d.version}`);
  if (d.algorithm === 'pbkdf2-sha256') {
    if (d.iterations < MIN_ITERATIONS || d.iterations > MAX_ITERATIONS) {
      errs.push(`iterations ${d.iterations} out of range [${MIN_ITERATIONS}, ${MAX_ITERATIONS}]`);
    }
  } else if (d.algorithm === 'hmac-sha256' && d.iterations !== 0) {
    errs.push(`hmac-sha256 carries no work factor but iterations=${d.iterations}`);
  }
  if (d.salt.length === 0) errs.push('digest salt must be non-empty');
  if (d.hash.length === 0) errs.push('digest hash must be non-empty');
  return errs;
}

/** Renders the canonical PHC wire form of a digest (mirror of keyring Digest.String). */
function encodeDigestString(d: Digest): string {
  const salt = encodeBase64RawStd(d.salt);
  const hash = encodeBase64RawStd(d.hash);
  if (d.algorithm === 'pbkdf2-sha256') {
    return `$${d.algorithm}$v=${d.version}$i=${d.iterations}$${salt}$${hash}`;
  }
  return `$${d.algorithm}$v=${d.version}$${salt}$${hash}`;
}

// --- Hashing core ------------------------------------------------------------

/**
 * Derives the raw digest bytes for a secret under a given algorithm/salt/
 * iterations. Deterministic core shared by {@link hashSecret} and
 * {@link verifySecret}: identical inputs always yield identical bytes, which is
 * what makes the cross-language vectors reproducible.
 *
 * - pbkdf2-sha256: PBKDF2-HMAC-SHA256 over the UTF-8 secret and salt → 32 bytes.
 * - hmac-sha256: keyed HMAC-SHA256 with **key = salt, message = secret** (the
 *   same order as Go — reversing it would break byte-parity) → 32 bytes.
 */
async function computeHash(
  algorithm: DigestAlgorithm,
  secret: string,
  salt: Uint8Array<ArrayBuffer>,
  iterations: number,
): Promise<Uint8Array<ArrayBuffer>> {
  const message = TEXT_ENCODER.encode(secret);
  if (algorithm === 'pbkdf2-sha256') {
    const key = await crypto.subtle.importKey('raw', message, 'PBKDF2', false, ['deriveBits']);
    const bits = await crypto.subtle.deriveBits(
      { name: 'PBKDF2', salt, iterations, hash: 'SHA-256' },
      key,
      HASH_LENGTH * 8,
    );
    return new Uint8Array(bits);
  }
  // hmac-sha256: key = salt, message = secret.
  const key = await crypto.subtle.importKey('raw', salt, { name: 'HMAC', hash: 'SHA-256' }, false, ['sign']);
  const sig = await crypto.subtle.sign('HMAC', key, message);
  return new Uint8Array(sig);
}

/** Clamps an iteration count into [{@link ITERATION_FLOOR}, {@link MAX_ITERATIONS}]. */
function clampIterations(n: number): number {
  if (n < ITERATION_FLOOR) return ITERATION_FLOOR;
  if (n > MAX_ITERATIONS) return MAX_ITERATIONS;
  return n;
}

interface ResolvedConfig {
  algorithm: DigestAlgorithm;
  iterations: number;
}

function resolveConfig(opts: HashOptions): ResolvedConfig {
  const algorithm = opts.algorithm ?? 'pbkdf2-sha256';
  const iterations = opts.iterations === undefined ? DEFAULT_PBKDF2_ITERATIONS : clampIterations(opts.iterations);
  return { algorithm, iterations };
}

/**
 * Computes the digest for the given algorithm/salt/iterations and renders it
 * through the keyring grammar, validating first so a malformed digest can never
 * be emitted. Iterations is stamped only for pbkdf2-sha256.
 */
async function encodeDigest(
  algorithm: DigestAlgorithm,
  secret: string,
  salt: Uint8Array<ArrayBuffer>,
  iterations: number,
): Promise<string> {
  const hash = await computeHash(algorithm, secret, salt, iterations);
  const digest: Digest = {
    algorithm,
    version: DIGEST_VERSION,
    iterations: algorithm === 'pbkdf2-sha256' ? iterations : 0,
    salt,
    hash,
  };
  const problems = validateDigest(digest);
  if (problems.length > 0) {
    throw new Error(`security: constructed digest failed validation: ${problems.join('; ')}`);
  }
  return encodeDigestString(digest);
}

// --- Public API --------------------------------------------------------------

/**
 * Derives a versioned PHC-style digest for `secret` and returns its canonical
 * keyring wire form. A fresh random salt (from `crypto.getRandomValues`) is
 * drawn for every call, so two calls on the same secret produce different
 * digests; {@link verifySecret} recovers the salt from the stored digest. The
 * returned string always parses clean through {@link parseDigest}. The secret is
 * never logged. Byte-for-byte identical to the Go HashSecret twin.
 */
export async function hashSecret(secret: string, opts: HashOptions = {}): Promise<string> {
  const { algorithm, iterations } = resolveConfig(opts);
  const salt = new Uint8Array(DEFAULT_SALT_LENGTH);
  crypto.getRandomValues(salt);
  return encodeDigest(algorithm, secret, salt, iterations);
}

/**
 * INTERNAL test-only seam that pins the salt so the deterministic cross-language
 * vectors can be reproduced. It is the analogue of Go's unexported `withRand`
 * seam: production callers use {@link hashSecret}, which always draws a fresh
 * random salt, so a non-random salt cannot be injected in production.
 *
 * @internal
 */
export async function _hashWithSalt(
  secret: string,
  salt: Uint8Array<ArrayBuffer>,
  opts: HashOptions = {},
): Promise<string> {
  const { algorithm, iterations } = resolveConfig(opts);
  return encodeDigest(algorithm, secret, salt, iterations);
}

/**
 * Reports whether a validated stored digest should be re-hashed against the
 * current preset after a successful verify. For pbkdf2-sha256 that is an
 * iteration count below {@link DEFAULT_PBKDF2_ITERATIONS}; hmac-sha256 carries no
 * work factor and never needs a rehash on its own. Mirror of Go's needsRehash.
 */
function needsRehash(d: Digest): boolean {
  if (d.algorithm === 'pbkdf2-sha256') return d.iterations < DEFAULT_PBKDF2_ITERATIONS;
  return false;
}

/**
 * Checks `secret` against a stored PHC digest string. Parses and validates the
 * digest through the shared keyring grammar, recomputes the hash with the
 * digest's own salt/iterations, and compares in CONSTANT TIME. A digest that
 * fails to parse or validate, a recompute error, or a mismatched secret all
 * return `{ valid: false, rehashRequired: false }` (fail closed). On a match,
 * `rehashRequired` reports whether the stored digest is weaker than the current
 * preset. The secret is never logged. Mirror of Go's VerifySecret.
 */
export async function verifySecret(secret: string, digestString: string): Promise<VerifyResult> {
  const d = parseDigest(digestString);
  if (!d) return { valid: false, rehashRequired: false };
  let computed: Uint8Array;
  try {
    computed = await computeHash(d.algorithm, secret, d.salt, d.iterations);
  } catch {
    return { valid: false, rehashRequired: false };
  }
  if (!timingSafeEqualBytes(computed, d.hash)) return { valid: false, rehashRequired: false };
  return { valid: true, rehashRequired: needsRehash(d) };
}
