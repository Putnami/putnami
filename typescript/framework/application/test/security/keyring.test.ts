import { describe, expect, it } from 'bun:test';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { decodeJwtHeader, importPublicKey, verifyJwt } from '../../src/oauth/oauth-jwt';
import {
  allowedTransitions,
  canTransition,
  createSigningKeyProvider,
  generateSigningKey,
  isTerminal,
  KEY_STATES,
  NoSigningKeyError,
  signingKeyFromPrivateJwk,
  type PrivateKeyring,
  signingKeysFromKeyring,
} from '../../src/security/keyring';

// The shared, language-neutral corpus the Go conformance suite
// (protocols/keyring/conformance_test.go) also drives. Byte-for-byte parity with
// the projected JWKS and the key-state machine is the cross-language contract of
// the two implementations.
const FIXTURES = join(__dirname, '../../../../../protocols/keyring/fixtures');

function loadFixture<T>(name: string): T {
  return JSON.parse(readFileSync(join(FIXTURES, name), 'utf8')) as T;
}

// Verifies a compact JWS against a provider's published JWKS, returning the
// decoded payload. Selects the JWK by the token header's kid, exactly as an
// external verifier would.
async function verifyAgainstJwks<T>(
  provider: { publicJwks: () => { keys: Array<{ kid?: string }> } },
  token: string,
): Promise<T> {
  const header = decodeJwtHeader(token);
  const jwk = provider.publicJwks().keys.find((k) => k.kid === header?.kid);
  if (!jwk) throw new Error(`no published JWK for kid ${header?.kid}`);
  const key = await importPublicKey({ kind: 'jwk', jwk: jwk as JsonWebKey & { kid?: string } });
  return verifyJwt<T>(token, key, {});
}

describe('keyring key-state machine (cross-language corpus)', () => {
  interface KeyStateCorpus {
    states: string[];
    terminal: string[];
    transitions: { id: string; from: string; to: string; legal: boolean }[];
  }
  const corpus = loadFixture<KeyStateCorpus>('key-states.json');

  it('exposes the closed enum in canonical order', () => {
    expect(corpus.states).toEqual([...KEY_STATES]);
  });

  it('marks exactly the terminal states terminal', () => {
    for (const s of KEY_STATES) {
      expect(isTerminal(s)).toBe(corpus.terminal.includes(s));
    }
  });

  for (const t of corpus.transitions) {
    it(`transition ${t.id} is ${t.legal ? 'legal' : 'illegal'}`, () => {
      expect(canTransition(t.from, t.to)).toBe(t.legal);
    });
  }

  it('revoked is terminal with no allowed transitions', () => {
    expect(allowedTransitions('revoked')).toEqual([]);
    expect(allowedTransitions('active')).toEqual(['expired', 'retiring', 'revoked']);
  });
});

describe('keyring public JWKS projection (cross-language corpus)', () => {
  interface KeyringCorpus {
    cases: { id: string; private: PrivateKeyring; public: { keys: unknown[] } }[];
  }
  const corpus = loadFixture<KeyringCorpus>('keyrings.json');

  for (const c of corpus.cases) {
    it(`${c.id}: projects to the exact fixture public JWKS`, async () => {
      const keys = await signingKeysFromKeyring(c.private);
      // A fixed clock keeps the retiring key inside its overlap window at read time.
      const provider = await createSigningKeyProvider({ keys, clock: () => 1_700_000_000_000 });
      const jwks = provider.publicJwks();
      expect(jwks).toEqual(c.public as typeof jwks);

      // Belt-and-suspenders: the serialized projection leaks no private material or
      // keyring lifecycle state (mirror of the Go projection-corpus assertion).
      const raw = JSON.stringify(jwks);
      for (const field of ['d', 'p', 'q', 'dp', 'dq', 'qi', 'k']) {
        expect(raw).not.toContain(`"${field}":`);
      }
      expect(raw).not.toContain('"state":');
    });
  }
});

describe('keyring provider fail-closed construction', () => {
  it('fails closed with no key material and allowEphemeral false', async () => {
    await expect(createSigningKeyProvider({})).rejects.toBeInstanceOf(NoSigningKeyError);
  });

  it('mints an ephemeral active key only when explicitly allowed', async () => {
    const provider = await createSigningKeyProvider({ allowEphemeral: true });
    expect(provider.activeAlg()).toBe('ES256');
    expect(provider.activeKid().startsWith('ephemeral-')).toBe(true);
    expect(provider.publicJwks().keys).toHaveLength(1);
    // The ephemeral key can actually sign a verifiable token.
    const token = await provider.sign({ sub: 'ephemeral-user', exp: 4_102_444_800 });
    const payload = await verifyAgainstJwks<{ sub?: string }>(provider, token);
    expect(payload.sub).toBe('ephemeral-user');
  });

  it('rejects a keyring with more than one active key', async () => {
    const a = await generateSigningKey({ kid: 'a' });
    const b = await generateSigningKey({ kid: 'b' });
    await expect(createSigningKeyProvider({ keys: [a, b] })).rejects.toThrow(/exactly one active/);
  });

  it('rejects duplicate kids', async () => {
    const a = await generateSigningKey({ kid: 'dup' });
    const b = await generateSigningKey({ kid: 'dup', state: 'retiring' });
    await expect(createSigningKeyProvider({ keys: [a, b] })).rejects.toThrow(/duplicate kid/);
  });

  it('rejects supplied keys with no active key', async () => {
    const retiring = await generateSigningKey({ kid: 'r', state: 'retiring' });
    await expect(createSigningKeyProvider({ keys: [retiring] })).rejects.toThrow(/none is active/);
  });
});

describe('keyring persisted key validation', () => {
  interface KeyringCorpus {
    cases: { private: PrivateKeyring }[];
  }
  const corpus = loadFixture<KeyringCorpus>('keyrings.json');
  const fixtureKeys = corpus.cases[0]?.private.keys ?? [];

  it('rejects an EC key declared as RS256 before the provider can start', async () => {
    const ec = fixtureKeys.find((key) => key.kty === 'EC');
    if (!ec) throw new Error('missing EC fixture key');
    await expect(signingKeyFromPrivateJwk({ ...ec, alg: 'RS256' })).rejects.toThrow(/must declare ES256/);
  });

  it('rejects an RSA key declared as ES256 before the provider can start', async () => {
    const rsa = fixtureKeys.find((key) => key.kty === 'RSA');
    if (!rsa) throw new Error('missing RSA fixture key');
    await expect(signingKeyFromPrivateJwk({ ...rsa, alg: 'ES256' })).rejects.toThrow(/must declare RS256/);
  });
});

describe('keyring sign → verify round-trip', () => {
  it('ES256: a signed token verifies against the published JWKS', async () => {
    const key = await generateSigningKey({ kid: 'es-1' });
    const provider = await createSigningKeyProvider({ keys: [key] });
    expect(provider.activeAlg()).toBe('ES256');
    const token = await provider.sign({ sub: 'user-1', iss: 'putnami', exp: 4_102_444_800 });
    const payload = await verifyAgainstJwks<{ sub?: string; iss?: string }>(provider, token);
    expect(payload.sub).toBe('user-1');
    expect(payload.iss).toBe('putnami');
  });

  it('RS256: a signed token verifies against the published JWKS', async () => {
    const key = await generateSigningKey({ kid: 'rs-1', alg: 'RS256' });
    const provider = await createSigningKeyProvider({ keys: [key] });
    expect(provider.activeAlg()).toBe('RS256');
    const token = await provider.sign({ sub: 'user-2', exp: 4_102_444_800 });
    const payload = await verifyAgainstJwks<{ sub?: string }>(provider, token);
    expect(payload.sub).toBe('user-2');
  });
});

describe('keyring public JWKS isolation', () => {
  it('does not retain or expose mutable public-JWK references', async () => {
    const key = await generateSigningKey({ kid: 'isolated' });
    const provider = await createSigningKeyProvider({ keys: [key] });

    key.publicJwk.kid = 'mutated-input';
    const response = provider.publicJwks();
    response.keys[0]!.kid = 'mutated-response';

    expect(provider.publicJwks().keys[0]?.kid).toBe('isolated');
    const token = await provider.sign({ sub: 'immutable-jwks', exp: 4_102_444_800 });
    const payload = await verifyAgainstJwks<{ sub?: string }>(provider, token);
    expect(payload.sub).toBe('immutable-jwks');
  });
});

describe('keyring atomic rotation and overlap', () => {
  it('rotate demotes the old key to retiring; both keys verify during overlap', async () => {
    const keyA = await generateSigningKey({ kid: 'A' });
    const keyB = await generateSigningKey({ kid: 'B' });
    const provider = await createSigningKeyProvider({ keys: [keyA] });

    // Snapshot atomicity: sign() captures the active key (A) synchronously, then a
    // rotate() swaps in B before the async sign completes. The token is still signed
    // by A — a reader never observes a partially-rotated keyring.
    const pending = provider.sign({ sub: 'signed-by-A', exp: 4_102_444_800 });
    provider.rotate(keyB);
    const tokenA = await pending;

    expect(provider.activeKid()).toBe('B');
    // Both the new active key and the retiring key are published during the overlap.
    expect(
      provider
        .publicJwks()
        .keys.map((k) => k.kid)
        .sort(),
    ).toEqual(['A', 'B']);

    const payloadA = await verifyAgainstJwks<{ sub?: string }>(provider, tokenA);
    expect(payloadA.sub).toBe('signed-by-A');

    const tokenB = await provider.sign({ sub: 'signed-by-B', exp: 4_102_444_800 });
    const payloadB = await verifyAgainstJwks<{ sub?: string }>(provider, tokenB);
    expect(payloadB.sub).toBe('signed-by-B');
  });

  it('a retiring key past the overlap window drops from the published JWKS', async () => {
    let now = 1_000_000;
    const overlapWindowMs = 1000;
    const keyA = await generateSigningKey({ kid: 'A' });
    const keyB = await generateSigningKey({ kid: 'B' });
    const provider = await createSigningKeyProvider({ keys: [keyA], overlapWindowMs, clock: () => now });

    provider.rotate(keyB); // A retires at now
    expect(
      provider
        .publicJwks()
        .keys.map((k) => k.kid)
        .sort(),
    ).toEqual(['A', 'B']);

    now += overlapWindowMs + 1; // advance past the overlap window
    // Read-time window guard: A is swept from the JWKS even without another rotation.
    expect(provider.publicJwks().keys.map((k) => k.kid)).toEqual(['B']);
  });

  it('revoke removes a non-active key; the active key cannot be revoked directly', async () => {
    const keyA = await generateSigningKey({ kid: 'A' });
    const keyB = await generateSigningKey({ kid: 'B' });
    const provider = await createSigningKeyProvider({ keys: [keyA] });
    provider.rotate(keyB); // A now retiring, B active

    expect(() => provider.revoke('B')).toThrow(/cannot revoke the active key/);
    expect(() => provider.revoke('missing')).toThrow(/no key with kid/);

    provider.revoke('A'); // A is retiring → revoked
    expect(provider.publicJwks().keys.map((k) => k.kid)).toEqual(['B']);
  });
});
