// TypeScript signing keyring: the SIGN-side twin of go/framework/security
// (keyring.go + sign.go). It mints compact JWS tokens under a single active key,
// rotates keys atomically, and publishes the public JWKS a verifier consumes.
//
// This is the cross-language partner of the Go keyring. It mirrors
// the same vocabulary as go.putnami.dev/protocol/keyring — the closed key-state
// machine, the private→public JWK projection, and the publishability rule — and
// the same provider contract as go/framework/security.SigningKeyProvider: the
// fail-closed ephemeral policy, the exactly-one-active invariant, the
// copy-on-write atomic rotation, and the bounded retiring-key overlap window.
//
// Crypto is hand-rolled against WebCrypto and the JWS wire format. ES256 is
// P-256 + SHA-256 with the raw R||S signature (64 bytes, RFC 7518 §3.4) —
// exactly what crypto.subtle.sign/verify produce and consume, and
// byte-compatible with the Go signES256/verifyECDSA path — so a token minted
// here verifies against the Go keyring and vice versa. No private key
// material is ever logged or projected into the public JWKS.
//
// The key-state machine and JWK shapes are re-implemented here, not imported.
// Keep these constants and the transition table in sync with
// go.putnami.dev/protocol/keyring.

const TEXT_ENCODER = new TextEncoder();

// --- Protocol constants (mirror go.putnami.dev/protocol/keyring) -------------

/** Current keyring-protocol version (keyring.ProtocolVersion). */
export const KEYRING_PROTOCOL_VERSION = 1;

/**
 * Default retirement overlap: how long a rotated-out (retiring) key stays in the
 * published JWKS so tokens signed just before a rotation still verify during the
 * rollover. Mirror of go/framework/security.DefaultOverlapWindow (24h).
 */
export const DEFAULT_OVERLAP_WINDOW_MS = 24 * 60 * 60 * 1000;

/** ECDSA using P-256 and SHA-256 (RFC 7518 §3.4); the raw R||S JWS signature. */
export const ALG_ES256 = 'ES256';
/** RSASSA-PKCS1-v1_5 using SHA-256 (RFC 7518 §3.3). */
export const ALG_RS256 = 'RS256';

/** The JWS signing algorithms this keyring can mint under. */
export type SigningAlg = typeof ALG_ES256 | typeof ALG_RS256;

// --- Key-state lifecycle (mirror keyring.KeyState) ---------------------------

/**
 * The lifecycle state of a signing key. The enum is intentionally closed and
 * matches keyring.KeyState.
 */
export type KeyState = 'active' | 'retiring' | 'revoked' | 'expired';

/** Every key state in canonical order (mirror keyring.AllKeyStates). */
export const KEY_STATES: readonly KeyState[] = ['active', 'retiring', 'revoked', 'expired'];

/**
 * The closed legal-transition table for the key-state machine (mirror
 * keyring.legalTransitions). Revocation is reachable from any non-revoked state
 * and revoked is the only terminal state; a same-state edge is not in the table
 * and is therefore rejected.
 */
const LEGAL_TRANSITIONS: Record<KeyState, Partial<Record<KeyState, true>>> = {
  active: { retiring: true, expired: true, revoked: true },
  retiring: { expired: true, revoked: true },
  expired: { revoked: true },
  revoked: {},
};

/** Reports whether s is a recognized key state (mirror KeyState.Valid). */
export function isValidKeyState(s: string): s is KeyState {
  return s === 'active' || s === 'retiring' || s === 'revoked' || s === 'expired';
}

/**
 * Reports whether a key in this state belongs in the PUBLIC JWKS: only active and
 * retiring keys are publishable (mirror KeyState.Publishable). This is the
 * fail-closed rule the projection bakes in so a revoked or expired key is never
 * advertised.
 */
export function isPublishable(s: KeyState): boolean {
  return s === 'active' || s === 'retiring';
}

/**
 * Reports whether a key may legally move from state `from` to state `to`. Returns
 * false for an unknown state and for a same-state edge (mirror
 * keyring.CanTransition).
 */
export function canTransition(from: string, to: string): boolean {
  if (!isValidKeyState(from) || !isValidKeyState(to)) return false;
  return LEGAL_TRANSITIONS[from][to] === true;
}

/** Reports whether state s permits no further transitions (mirror KeyState.Terminal). Only revoked is terminal. */
export function isTerminal(s: KeyState): boolean {
  return Object.keys(LEGAL_TRANSITIONS[s]).length === 0;
}

/** The states s may legally transition to, sorted (mirror KeyState.AllowedTransitions). */
export function allowedTransitions(s: KeyState): KeyState[] {
  return (Object.keys(LEGAL_TRANSITIONS[s]) as KeyState[]).sort();
}

// --- JWK shapes (mirror keyring.JWK / keyring.PrivateJWK) ---------------------

/**
 * A PUBLIC JSON Web Key: public verification material only. Field names follow
 * standard JOSE and match keyring.JWK, so a verifier (oauth-jwt.ts) and the Go
 * ParseJWKS consume it directly. It has NO private-material fields, so it
 * structurally cannot carry a private key.
 */
export interface Jwk {
  kty: string;
  kid?: string;
  use?: string;
  alg?: string;
  crv?: string;
  x?: string;
  y?: string;
  n?: string;
  e?: string;
}

/** A PUBLIC JSON Web Key Set document (mirror keyring.JWKS). */
export interface Jwks {
  keys: Jwk[];
}

/**
 * A full JSON Web Key that MAY carry private key material and a keyring lifecycle
 * state (mirror keyring.PrivateJWK). It is the owner-side shape; a verifier only
 * ever receives its public projection. Never serialized into a public JWKS.
 */
export interface PrivateJwk extends Jwk {
  state?: KeyState;
  /** RFC 3339 instant a retiring key entered its overlap window. Keyring-local; never projected. */
  retiredAt?: string;
  // Private material — NEVER projected to the public JWK.
  d?: string;
  p?: string;
  q?: string;
  dp?: string;
  dq?: string;
  qi?: string;
  k?: string;
}

/** A private-keyring document (mirror keyring.PrivateKeyring). */
export interface PrivateKeyring {
  $schema?: string;
  protocolVersion: number;
  keys: PrivateJwk[];
}

// --- Fail-closed sentinel ----------------------------------------------------

/**
 * The FAIL-CLOSED error thrown when a provider is constructed with no signing key
 * material and ephemeral generation was not explicitly allowed. A serverless cold
 * start with no configured key throws this rather than silently minting tokens
 * under an untrusted, process-local key. Mirror of Go's ErrNoSigningKey.
 */
export class NoSigningKeyError extends Error {
  constructor() {
    super(
      'security: no signing key configured and allowEphemeral is false ' +
        '(refusing to mint tokens under an untrusted ephemeral key)',
    );
    this.name = 'NoSigningKeyError';
  }
}

// --- Base64url (RFC 4648 §5, unpadded — matches Go base64.RawURLEncoding) -----

function b64uBytes(bytes: Uint8Array): string {
  let bin = '';
  for (let i = 0; i < bytes.length; i++) bin += String.fromCharCode(bytes[i]);
  return btoa(bin).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

function b64uString(s: string): string {
  return b64uBytes(TEXT_ENCODER.encode(s));
}

// --- Managed key -------------------------------------------------------------

/**
 * One managed signing key: its JOSE identity (kid, alg), its keyring lifecycle
 * state, the WebCrypto private key used to sign, and the already-stripped public
 * JWK used for publication. A managed key is treated as IMMUTABLE once published
 * in a provider state — a state change produces a new value (see withState)
 * rather than mutating in place — which is what makes the lock-free read path
 * race-free under the copy-on-write swap. The stored publicJwk carries no private
 * material and no lifecycle state, so the projection can never leak either.
 */
export interface SigningKey {
  readonly kid: string;
  readonly alg: SigningAlg;
  readonly state: KeyState;
  /** ms epoch when the key entered "retiring"; 0 otherwise. */
  readonly retiredAt: number;
  /** True only for a process-generated ephemeral key. */
  readonly ephemeral: boolean;
  /** @internal WebCrypto private key. Never logged or projected. */
  readonly privateKey: CryptoKey;
  /** @internal Public JWK for publication (no private material, no state). */
  readonly publicJwk: Jwk;
}

function withState(k: SigningKey, state: KeyState, retiredAt: number): SigningKey {
  return { ...k, state, retiredAt };
}

/** Returns a detached copy so public-JWKS consumers cannot mutate provider state. */
function copyPublicJwk(jwk: Jwk): Jwk {
  return { ...jwk };
}

/**
 * Assembles a compact JWS (header.payload.signature) over the payload using this
 * key. The header carries alg, typ:"JWT" and kid in the same field order as the
 * Go joseHeader, and the signature is the alg's raw JWS signature — for ES256 the
 * 64-byte R||S form crypto.subtle.sign already returns.
 */
async function signWith(k: SigningKey, payload: Uint8Array): Promise<string> {
  const header = JSON.stringify({ alg: k.alg, typ: 'JWT', kid: k.kid });
  const signingInput = `${b64uString(header)}.${b64uBytes(payload)}`;
  const data = TEXT_ENCODER.encode(signingInput);
  let sig: ArrayBuffer;
  if (k.alg === ALG_ES256) {
    sig = await crypto.subtle.sign({ name: 'ECDSA', hash: 'SHA-256' }, k.privateKey, data);
  } else {
    sig = await crypto.subtle.sign('RSASSA-PKCS1-v1_5', k.privateKey, data);
  }
  return `${signingInput}.${b64uBytes(new Uint8Array(sig))}`;
}

// --- Public-key projection ---------------------------------------------------

/**
 * Projects a WebCrypto public key to a JOSE public JWK, copying only public
 * parameters. The result structurally cannot carry private material.
 */
async function exportPublicJwk(publicKey: CryptoKey, alg: SigningAlg, kid: string): Promise<Jwk> {
  const jwk = await crypto.subtle.exportKey('jwk', publicKey);
  if (alg === ALG_ES256) {
    return { kty: 'EC', kid, use: 'sig', alg, crv: 'P-256', x: jwk.x ?? '', y: jwk.y ?? '' };
  }
  return { kty: 'RSA', kid, use: 'sig', alg, n: jwk.n ?? '', e: jwk.e ?? '' };
}

/**
 * Builds the public JWK from an owner-side private JWK, copying only the fields
 * present in the source so a projected key is byte-identical to what
 * keyring.PublicJWKS would emit (and to the shared keyrings.json fixture). Never
 * copies a private field or the lifecycle state.
 */
function publicJwkFromPrivate(pj: PrivateJwk): Jwk {
  const out: Jwk = { kty: pj.kty };
  if (pj.kid !== undefined) out.kid = pj.kid;
  if (pj.use !== undefined) out.use = pj.use;
  if (pj.alg !== undefined) out.alg = pj.alg;
  if (pj.kty === 'EC') {
    if (pj.crv !== undefined) out.crv = pj.crv;
    if (pj.x !== undefined) out.x = pj.x;
    if (pj.y !== undefined) out.y = pj.y;
  } else if (pj.kty === 'RSA') {
    if (pj.n !== undefined) out.n = pj.n;
    if (pj.e !== undefined) out.e = pj.e;
  }
  return out;
}

// --- Key factories -----------------------------------------------------------

/** Options for {@link generateSigningKey}. */
export interface GenerateSigningKeyOptions {
  /** JWS algorithm. Defaults to ES256 (P-256). */
  alg?: SigningAlg;
  /** Key ID. When omitted, a stable thumbprint kid is derived from the public key. */
  kid?: string;
  /** Initial lifecycle state. Defaults to active. */
  state?: KeyState;
}

/**
 * Generates a fresh in-memory signing key via crypto.subtle.generateKey. ES256
 * (default) is P-256; RS256 is RSA-2048. The public key is exported to a public
 * JWK immediately so the private material stays inside the returned key. When no
 * kid is supplied a stable thumbprint kid is derived, so the same public key
 * always advertises the same kid.
 */
export async function generateSigningKey(opts: GenerateSigningKeyOptions = {}): Promise<SigningKey> {
  const alg = opts.alg ?? ALG_ES256;
  const state = opts.state ?? 'active';
  let pair: CryptoKeyPair;
  if (alg === ALG_ES256) {
    pair = (await crypto.subtle.generateKey({ name: 'ECDSA', namedCurve: 'P-256' }, true, [
      'sign',
      'verify',
    ])) as CryptoKeyPair;
  } else {
    pair = (await crypto.subtle.generateKey(
      { name: 'RSASSA-PKCS1-v1_5', modulusLength: 2048, publicExponent: new Uint8Array([1, 0, 1]), hash: 'SHA-256' },
      true,
      ['sign', 'verify'],
    )) as CryptoKeyPair;
  }
  const rawPublic = await crypto.subtle.exportKey('jwk', pair.publicKey);
  const kid = opts.kid ?? (await deriveThumbprintKid(alg, rawPublic, 'key-'));
  const publicJwk = await exportPublicJwk(pair.publicKey, alg, kid);
  return { kid, alg, state, retiredAt: 0, ephemeral: false, privateKey: pair.privateKey, publicJwk };
}

/**
 * Mints a throwaway ephemeral ES256 (P-256) active key. Its kid is a
 * deterministic thumbprint of the public point prefixed "ephemeral-", mirroring
 * Go's generateEphemeralKey, so the same key always advertises the same kid.
 */
async function generateEphemeralKey(): Promise<SigningKey> {
  const pair = (await crypto.subtle.generateKey({ name: 'ECDSA', namedCurve: 'P-256' }, true, [
    'sign',
    'verify',
  ])) as CryptoKeyPair;
  const rawPublic = await crypto.subtle.exportKey('jwk', pair.publicKey);
  const kid = await deriveThumbprintKid(ALG_ES256, rawPublic, 'ephemeral-');
  const publicJwk = await exportPublicJwk(pair.publicKey, ALG_ES256, kid);
  return {
    kid,
    alg: ALG_ES256,
    state: 'active',
    retiredAt: 0,
    ephemeral: true,
    privateKey: pair.privateKey,
    publicJwk,
  };
}

/**
 * Derives a stable kid from the public key material (mirror Go's ephemeralKID for
 * EC keys): sha256("EC|P-256|x|y") truncated to 8 bytes, base64url-encoded, under
 * the given prefix. RSA keys thumbprint their n|e the same way.
 */
async function deriveThumbprintKid(alg: SigningAlg, rawPublic: JsonWebKey, prefix: string): Promise<string> {
  const material =
    alg === ALG_ES256
      ? `EC|P-256|${rawPublic.x ?? ''}|${rawPublic.y ?? ''}`
      : `RSA|${rawPublic.n ?? ''}|${rawPublic.e ?? ''}`;
  const digest = await crypto.subtle.digest('SHA-256', TEXT_ENCODER.encode(material));
  return prefix + b64uBytes(new Uint8Array(digest).subarray(0, 8));
}

/**
 * Reconstructs a signing key from an owner-side private JWK (the store-load path,
 * mirror Go's signingKeysFromKeyring per-key parse). The private key is imported
 * for signing only; the public JWK is projected from the source's public fields
 * so it is byte-identical to what the keyring publishes.
 */
export async function signingKeyFromPrivateJwk(pj: PrivateJwk): Promise<SigningKey> {
  if (!pj.kid) throw new Error('security: signing key requires a kid');
  const state = pj.state ?? 'active';
  if (!isValidKeyState(state)) throw new Error(`security: signing key ${pj.kid} has invalid state ${String(state)}`);
  const retiredAt = parseRetiredAt(pj.retiredAt);

  let alg: SigningAlg;
  let privateKey: CryptoKey;
  if (pj.kty === 'EC') {
    if (pj.crv !== 'P-256') throw new Error(`security: EC key ${pj.kid} must be P-256`);
    if (!pj.d || !pj.x || !pj.y) throw new Error(`security: EC key ${pj.kid} missing x/y/d`);
    if (pj.alg && pj.alg !== ALG_ES256) {
      throw new Error(`security: EC key ${pj.kid} must declare ${ALG_ES256}, got ${pj.alg}`);
    }
    alg = ALG_ES256;
    privateKey = await crypto.subtle.importKey(
      'jwk',
      { kty: 'EC', crv: 'P-256', x: pj.x, y: pj.y, d: pj.d },
      { name: 'ECDSA', namedCurve: 'P-256' },
      false,
      ['sign'],
    );
  } else if (pj.kty === 'RSA') {
    if (!pj.n || !pj.e || !pj.d || !pj.p || !pj.q || !pj.dp || !pj.dq || !pj.qi) {
      throw new Error(`security: RSA key ${pj.kid} missing a required parameter (n,e,d,p,q,dp,dq,qi)`);
    }
    if (pj.alg && pj.alg !== ALG_RS256) {
      throw new Error(`security: RSA key ${pj.kid} must declare ${ALG_RS256}, got ${pj.alg}`);
    }
    alg = ALG_RS256;
    privateKey = await crypto.subtle.importKey(
      'jwk',
      { kty: 'RSA', n: pj.n, e: pj.e, d: pj.d, p: pj.p, q: pj.q, dp: pj.dp, dq: pj.dq, qi: pj.qi, alg: 'RS256' },
      { name: 'RSASSA-PKCS1-v1_5', hash: 'SHA-256' },
      false,
      ['sign'],
    );
  } else {
    throw new Error(`security: unsupported key type ${pj.kty} for key ${pj.kid}`);
  }

  return { kid: pj.kid, alg, state, retiredAt, ephemeral: false, privateKey, publicJwk: publicJwkFromPrivate(pj) };
}

/**
 * Parses the publishable (active/retiring) keys of an owner keyring into signing
 * keys carrying WebCrypto private keys. Revoked, expired and symmetric (oct) keys
 * are skipped — they neither sign nor publish. Mirror of Go's
 * signingKeysFromKeyring.
 */
export async function signingKeysFromKeyring(kr: PrivateKeyring): Promise<SigningKey[]> {
  const out: SigningKey[] = [];
  for (const pj of kr.keys) {
    const state = pj.state ?? 'active';
    if (!isValidKeyState(state) || !isPublishable(state)) continue;
    if (pj.kty === 'oct') continue; // symmetric keys cannot sign a JWS verified via a public JWKS
    out.push(await signingKeyFromPrivateJwk({ ...pj, state }));
  }
  return out;
}

/** Parses an RFC 3339 retiredAt instant to ms epoch; empty/malformed yields 0. */
function parseRetiredAt(s: string | undefined): number {
  if (!s) return 0;
  const t = Date.parse(s);
  return Number.isNaN(t) ? 0 : t;
}

// --- Provider ----------------------------------------------------------------

/**
 * The IMMUTABLE snapshot a signer/publisher reads. It is replaced wholesale
 * (never mutated in place) on every rotation, so a reader always sees a
 * consistent set with exactly one active key — never zero, never two,
 * mid-rotation. Mirror of Go's providerState.
 */
interface ProviderState {
  keys: SigningKey[];
  activeKid: string;
}

/** Configures a {@link SigningKeyProvider}. */
export interface SigningKeyProviderConfig {
  /**
   * The set of managed signing keys. Exactly one must be in the active state; a
   * set with zero or more than one active key is rejected.
   */
  keys?: SigningKey[];
  /**
   * Bounds how long a retired key stays in the published JWKS. Defaults to
   * {@link DEFAULT_OVERLAP_WINDOW_MS} when omitted or non-positive.
   */
  overlapWindowMs?: number;
  /**
   * Permits generating an in-memory ephemeral active key when NO key material is
   * supplied. Default false: construction FAILS CLOSED with
   * {@link NoSigningKeyError} rather than minting tokens under an untrusted,
   * process-local key.
   */
  allowEphemeral?: boolean;
  /** @internal Test seam overriding the provider clock. Production uses Date.now. */
  clock?: () => number;
}

/**
 * An in-memory {@link SigningKeyProvider}: mints compact JWS tokens under a single
 * active key and publishes the public JWKS a verifier consumes. Writers (rotate,
 * revoke) publish a new provider state with a single synchronous reference swap;
 * readers (sign, publicJwks, activeKid) snapshot the current state before any
 * await. This copy-on-write swap is the atomic active-key switch, so an in-flight
 * sign never observes a partially-rotated keyring. Mirror of Go's
 * inMemoryProvider / SigningKeyProvider + RotatingSigningKeyProvider.
 */
export class SigningKeyProvider {
  private state: ProviderState;
  private readonly overlap: number;
  private readonly clock: () => number;

  private constructor(state: ProviderState, overlap: number, clock: () => number) {
    this.state = state;
    this.overlap = overlap;
    this.clock = clock;
  }

  /**
   * Builds a provider from config, enforcing the core invariants: EXACTLY ONE
   * active key, unique kids, and the fail-closed ephemeral policy (no key material
   * + allowEphemeral false ⇒ {@link NoSigningKeyError}). Mirror of Go's
   * NewSigningKeyProvider / newProvider.
   */
  static async create(config: SigningKeyProviderConfig = {}): Promise<SigningKeyProvider> {
    const overlap =
      config.overlapWindowMs && config.overlapWindowMs > 0 ? config.overlapWindowMs : DEFAULT_OVERLAP_WINDOW_MS;
    const clock = config.clock ?? Date.now;
    const supplied = config.keys ?? [];

    const keys: SigningKey[] = [];
    const seen = new Set<string>();
    let activeCount = 0;
    let activeKid = '';
    for (const sk of supplied) {
      const mk = normalizeKey(sk, clock);
      if (seen.has(mk.kid)) throw new Error(`security: duplicate kid ${mk.kid}`);
      seen.add(mk.kid);
      if (mk.state === 'active') {
        activeCount++;
        activeKid = mk.kid;
      }
      keys.push(mk);
    }

    if (activeCount > 1) {
      throw new Error(`security: keyring must have exactly one active key, found ${activeCount}`);
    }
    if (activeCount === 0) {
      if (supplied.length === 0 && config.allowEphemeral) {
        const mk = await generateEphemeralKey();
        keys.push(mk);
        activeKid = mk.kid;
      } else if (supplied.length === 0) {
        // No key material at all and ephemeral generation not allowed: fail closed.
        throw new NoSigningKeyError();
      } else {
        // Keys supplied but none is active: cannot sign. Do not silently generate one.
        throw new Error(`security: keyring has ${supplied.length} key(s) but none is active`);
      }
    }

    return new SigningKeyProvider({ keys, activeKid }, overlap, clock);
  }

  /**
   * Returns a compact JWS over `claims`, signed by the current active key. The
   * state is snapshotted synchronously before the async sign, so a concurrent
   * rotate/revoke never affects an in-flight sign. Mirror of Go's Sign.
   */
  async sign(claims: Record<string, unknown>): Promise<string> {
    const st = this.state;
    const active = st.keys.find((k) => k.kid === st.activeKid && k.state === 'active');
    if (!active) throw new Error('security: no active signing key');
    const payload = TEXT_ENCODER.encode(JSON.stringify(claims));
    return signWith(active, payload);
  }

  /** The kid of the current active signing key (mirror ActiveKID). */
  activeKid(): string {
    return this.state.activeKid;
  }

  /** The JWS alg of the current active signing key (mirror ActiveAlg). */
  activeAlg(): string {
    const st = this.state;
    const active = st.keys.find((k) => k.kid === st.activeKid && k.state === 'active');
    return active ? active.alg : '';
  }

  /** How long a retired key stays in the published JWKS, in ms (mirror OverlapWindow). */
  overlapWindowMs(): number {
    return this.overlap;
  }

  /**
   * The public JWKS: the active key plus every retiring key still inside the
   * overlap window. A retiring key past the window is treated as expired here — the
   * READ-TIME window guard that removes a stale key even without a rotation — and a
   * revoked/expired key is never published. The result carries no private material
   * and no lifecycle state. Mirror of Go's PublicJWKS.
   */
  publicJwks(): Jwks {
    const st = this.state;
    const now = this.clock();
    const keys: Jwk[] = [];
    for (const k of st.keys) {
      let state = k.state;
      if (state === 'retiring' && now - k.retiredAt > this.overlap) state = 'expired';
      if (isPublishable(state)) keys.push(copyPublicJwk(k.publicJwk));
    }
    return { keys };
  }

  /**
   * Promotes `newKey` to the single active key and demotes the previous active key
   * to retiring for the overlap window. Any retiring key already past the window is
   * swept out. The whole transition is published with one synchronous reference
   * swap, so a concurrent signer/publisher never observes zero or two active keys.
   * The demotion is validated against the closed key-state machine. Mirror of Go's
   * Rotate.
   */
  rotate(newKey: SigningKey): void {
    if (newKey.state !== 'active') throw new Error(`security: a rotated-in key must be active, got ${newKey.state}`);
    const mk = normalizeKey(newKey, this.clock);
    const old = this.state;
    const now = this.clock();

    const newKeys: SigningKey[] = [];
    for (const k of old.keys) {
      if (k.kid === mk.kid) throw new Error(`security: rotated-in kid ${mk.kid} already exists in the keyring`);
      if (k.state === 'active') {
        if (!canTransition('active', 'retiring')) throw new Error('security: illegal active→retiring transition');
        newKeys.push(withState(k, 'retiring', now));
      } else if (k.state === 'retiring') {
        // Past-window retiring keys leave the keyring (and thus the JWKS).
        if (now - k.retiredAt > this.overlap) continue;
        newKeys.push(k);
      }
      // Expired/revoked keys are neither signed with nor published: drop them.
    }
    newKeys.push(mk);
    this.state = { keys: newKeys, activeKid: mk.kid };
  }

  /**
   * Marks a NON-active key revoked (terminal); a revoked key leaves the published
   * JWKS immediately and never signs. The active key cannot be revoked directly —
   * that would leave the keyring with no active key — so rotate in a replacement
   * first. The transition is validated against the key-state machine. Mirror of
   * Go's Revoke.
   */
  revoke(kid: string): void {
    const old = this.state;
    let found = false;
    const newKeys: SigningKey[] = [];
    for (const k of old.keys) {
      if (k.kid !== kid) {
        newKeys.push(k);
        continue;
      }
      found = true;
      if (k.state === 'active') {
        throw new Error(`security: cannot revoke the active key ${kid}; rotate to a new active key first`);
      }
      if (!canTransition(k.state, 'revoked')) throw new Error(`security: cannot revoke key ${kid} in state ${k.state}`);
      newKeys.push(withState(k, 'revoked', k.retiredAt));
    }
    if (!found) throw new Error(`security: no key with kid ${kid}`);
    this.state = { keys: newKeys, activeKid: old.activeKid };
  }
}

/**
 * Validates a supplied signing key and returns its normalized form. A key
 * supplied already-retiring with no retirement instant starts its overlap window
 * now, so it is published during the window and swept out after it. Mirror of the
 * per-key checks in newManagedKey / newProvider.
 */
function normalizeKey(sk: SigningKey, clock: () => number): SigningKey {
  if (!sk.kid) throw new Error('security: signing key requires a kid');
  if (sk.alg !== ALG_ES256 && sk.alg !== ALG_RS256) {
    throw new Error(`security: unsupported signing alg ${String(sk.alg)} for key ${sk.kid}`);
  }
  if (!isValidKeyState(sk.state))
    throw new Error(`security: signing key ${sk.kid} has invalid state ${String(sk.state)}`);
  let retiredAt = sk.retiredAt;
  if (sk.state === 'retiring' && retiredAt === 0) retiredAt = clock();
  return { ...sk, retiredAt, publicJwk: copyPublicJwk(sk.publicJwk) };
}

/**
 * Builds a {@link SigningKeyProvider} from config. The primary functional entry
 * point; equivalent to {@link SigningKeyProvider.create}. Mirror of Go's
 * NewSigningKeyProvider.
 */
export function createSigningKeyProvider(config: SigningKeyProviderConfig = {}): Promise<SigningKeyProvider> {
  return SigningKeyProvider.create(config);
}
