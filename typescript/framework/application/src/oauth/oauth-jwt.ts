/**
 * JWT / crypto primitives for the OAuth client.
 *
 * Security-critical helpers extracted from {@link OAuthService} so the hand-rolled
 * RS256/ES256 verifiers, base64url decoding, public-key import and claim validation
 * can be reviewed and tested in isolation. The orchestration layer (discovery, JWKS
 * caching, token I/O) lives in `oauth.service.ts` and imports from here.
 *
 * The asymmetric verify path is the cross-language partner of the Go keyring's SIGN
 * path (`go/framework/security`): a token minted by a Putnami keyring —
 * RS256 or ES256 — verifies here byte-for-byte. ES256 is P-256 + SHA-256 with the
 * raw R||S signature (64 bytes, RFC 7518 §3.4), which is exactly what WebCrypto's
 * ECDSA verify consumes, so no DER unwrapping is needed.
 */

/**
 * JWT verification options. The signature algorithm is taken from the token's JOSE
 * header (RS256 or ES256); HS256 and any other alg are rejected.
 */
export type VerifyOptions = {
  audience?: string | RegExp | Array<string | RegExp>;
  issuer?: string | string[];
  clockTolerance?: number;
  ignoreExpiration?: boolean;
  ignoreNotBefore?: boolean;
};

export type PublicKeyMaterial = { kind: 'jwk'; jwk: JsonWebKey & { kid?: string } };

type JwtHeader = { alg?: string; typ?: string; kid?: string };
type JwtPayload = Record<string, unknown> & {
  iss?: string;
  aud?: string | string[];
  exp?: number;
  nbf?: number;
};

const RS256_ALGORITHM: RsaHashedImportParams = { name: 'RSASSA-PKCS1-v1_5', hash: 'SHA-256' };
const ES256_IMPORT_ALGORITHM: EcKeyImportParams = { name: 'ECDSA', namedCurve: 'P-256' };
const ES256_VERIFY_ALGORITHM: EcdsaParams = { name: 'ECDSA', hash: 'SHA-256' };
const TEXT_DECODER = new TextDecoder();
const TEXT_ENCODER = new TextEncoder();

// ---------------------------------------------------------------------------
// Base64url + crypto helpers
// ---------------------------------------------------------------------------

function decodeBase64ToBytes(b64: string): Uint8Array<ArrayBuffer> {
  const bin = atob(b64);
  const buffer = new ArrayBuffer(bin.length);
  const bytes = new Uint8Array(buffer);
  for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
  return bytes;
}

export function decodeBase64Url(input: string): Uint8Array<ArrayBuffer> {
  const pad = input.length % 4 === 0 ? '' : '='.repeat(4 - (input.length % 4));
  return decodeBase64ToBytes(input.replace(/-/g, '+').replace(/_/g, '/') + pad);
}

/**
 * Chooses the WebCrypto import parameters for a public JWK. Elliptic-curve keys
 * (kty `EC`, e.g. the keyring's ES256 signing keys) import as ECDSA P-256; every
 * other JWK imports as RSASSA-PKCS1-v1_5 (RS256), preserving the prior behaviour.
 * The returned CryptoKey is algorithm-bound, so {@link verifyJwt} must dispatch the
 * verify call on the token header's alg to match the imported key.
 */
function jwkImportAlgorithm(jwk: JsonWebKey): RsaHashedImportParams | EcKeyImportParams {
  return jwk.kty === 'EC' ? ES256_IMPORT_ALGORITHM : RS256_ALGORITHM;
}

export async function importPublicKey(material: PublicKeyMaterial): Promise<CryptoKey> {
  return crypto.subtle.importKey('jwk', material.jwk, jwkImportAlgorithm(material.jwk), false, ['verify']);
}

// ---------------------------------------------------------------------------
// JWT
// ---------------------------------------------------------------------------

function matchesAudience(expected: string | RegExp | Array<string | RegExp>, tokenAud: string | string[]): boolean {
  const expectations = Array.isArray(expected) ? expected : [expected];
  const candidates = Array.isArray(tokenAud) ? tokenAud : [tokenAud];
  return expectations.some((e) => candidates.some((c) => (e instanceof RegExp ? e.test(c) : e === c)));
}

function validateJwtClaims(payload: JwtPayload, options: VerifyOptions): void {
  const now = Math.floor(Date.now() / 1000);
  const tolerance = options.clockTolerance ?? 0;

  if (!options.ignoreExpiration && typeof payload.exp === 'number' && now > payload.exp + tolerance) {
    throw new Error('jwt expired');
  }
  if (!options.ignoreNotBefore && typeof payload.nbf === 'number' && now + tolerance < payload.nbf) {
    throw new Error('jwt not active');
  }
  if (options.issuer !== undefined) {
    const issuers = Array.isArray(options.issuer) ? options.issuer : [options.issuer];
    if (typeof payload.iss !== 'string' || !issuers.includes(payload.iss)) {
      throw new Error('jwt issuer mismatch');
    }
  }
  if (options.audience !== undefined) {
    if (payload.aud === undefined || !matchesAudience(options.audience, payload.aud)) {
      throw new Error('jwt audience mismatch');
    }
  }
}

export function decodeJwtHeader(token: string): JwtHeader | undefined {
  const parts = token.split('.');
  if (parts.length !== 3) return undefined;
  try {
    return JSON.parse(TEXT_DECODER.decode(decodeBase64Url(parts[0]))) as JwtHeader;
  } catch {
    return undefined;
  }
}

export async function verifyJwtRS256<T>(token: string, key: CryptoKey, options: VerifyOptions): Promise<T> {
  const parts = token.split('.');
  if (parts.length !== 3) throw new Error('invalid jwt format');
  const [headerB64, payloadB64, sigB64] = parts;

  const header = JSON.parse(TEXT_DECODER.decode(decodeBase64Url(headerB64))) as JwtHeader;
  if (header.alg !== 'RS256') throw new Error(`unsupported algorithm: ${header.alg ?? '<none>'}`);

  const signed = TEXT_ENCODER.encode(`${headerB64}.${payloadB64}`);
  const valid = await crypto.subtle.verify(RS256_ALGORITHM.name, key, decodeBase64Url(sigB64), signed);
  if (!valid) throw new Error('invalid signature');

  const payload = JSON.parse(TEXT_DECODER.decode(decodeBase64Url(payloadB64))) as JwtPayload;
  validateJwtClaims(payload, options);
  return payload as T;
}

/**
 * Verifies and decodes an ES256 (ECDSA P-256 + SHA-256) JWT against an imported EC
 * public key. The JWS signature is the raw R||S concatenation (64 bytes, RFC 7518
 * §3.4) — NOT DER — which is exactly the form WebCrypto's ECDSA verify consumes and
 * the Go keyring's signES256 emits, so a Go-signed ES256 token verifies here
 * unchanged. The `key` must have been imported with ECDSA P-256 parameters (see
 * {@link importPublicKey}).
 */
export async function verifyJwtES256<T>(token: string, key: CryptoKey, options: VerifyOptions): Promise<T> {
  const parts = token.split('.');
  if (parts.length !== 3) throw new Error('invalid jwt format');
  const [headerB64, payloadB64, sigB64] = parts;

  const header = JSON.parse(TEXT_DECODER.decode(decodeBase64Url(headerB64))) as JwtHeader;
  if (header.alg !== 'ES256') throw new Error(`unsupported algorithm: ${header.alg ?? '<none>'}`);

  const signed = TEXT_ENCODER.encode(`${headerB64}.${payloadB64}`);
  const valid = await crypto.subtle.verify(ES256_VERIFY_ALGORITHM, key, decodeBase64Url(sigB64), signed);
  if (!valid) throw new Error('invalid signature');

  const payload = JSON.parse(TEXT_DECODER.decode(decodeBase64Url(payloadB64))) as JwtPayload;
  validateJwtClaims(payload, options);
  return payload as T;
}

/**
 * Verifies an asymmetric JWT, dispatching on the token header's `alg`: RS256 →
 * {@link verifyJwtRS256}, ES256 → {@link verifyJwtES256}. HS256 (symmetric) and any
 * unrecognized algorithm are rejected — a public-JWKS verifier must never accept a
 * symmetric token (algorithm-confusion defense). The `key` must have been imported
 * with parameters matching the header alg (see {@link importPublicKey}).
 */
export async function verifyJwt<T>(token: string, key: CryptoKey, options: VerifyOptions): Promise<T> {
  const header = decodeJwtHeader(token);
  switch (header?.alg) {
    case 'RS256':
      return verifyJwtRS256<T>(token, key, options);
    case 'ES256':
      return verifyJwtES256<T>(token, key, options);
    default:
      throw new Error(`unsupported algorithm: ${header?.alg ?? '<none>'}`);
  }
}
