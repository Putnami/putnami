import { describe, expect, it } from 'bun:test';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { specTest } from '@putnami/runtime/spectest';
import { decodeBase64Url, importPublicKey, verifyJwt, verifyJwtES256, verifyJwtRS256 } from '../../src/oauth/oauth-jwt';

const forgedToken = {
  feature: 'typescript/authentication-authorization',
  requirement: 'oauth-token-validation',
  check: 'a-tampered-or-confused-token-is-refused',
};

// The shared, language-neutral corpus the Go conformance suite
// (protocols/keyring/conformance_test.go → TestJWTVectors) also drives. The
// `valid` tokens are REAL ES256/RS256 signatures produced by the Go keyring; this
// TS slice must cryptographically verify them byte-for-byte, and reject every
// `invalid` vector on the same structural grounds the Go suite pins.
const JWT_VECTORS_PATH = join(__dirname, '../../../../../protocols/keyring/fixtures/jwt-vectors.json');

interface JwtVector {
  id: string;
  algorithm: string;
  kid: string;
  token: string;
  jwk: Record<string, unknown> & { kty?: string; kid?: string; alg?: string };
  reason?: string;
}

interface JwtCorpus {
  payload: string;
  valid: JwtVector[];
  invalid: JwtVector[];
}

function loadCorpus(): JwtCorpus {
  const doc = JSON.parse(readFileSync(JWT_VECTORS_PATH, 'utf8')) as JwtCorpus;
  expect(doc.valid.length).toBeGreaterThan(0);
  expect(doc.invalid.length).toBeGreaterThan(0);
  return doc;
}

const TEXT_DECODER = new TextDecoder();
const PRIVATE_JWK_FIELDS = ['d', 'p', 'q', 'dp', 'dq', 'qi', 'k'];

// Mirror of Go's structuralJWTCheck: three non-empty base64url segments, an
// asymmetric header alg, a public JWK carrying no private material, and a header
// alg/kid that agree with the JWK. A non-empty result means the vector is
// structurally invalid.
function structuralProblems(v: JwtVector): string[] {
  const problems: string[] = [];
  const parts = v.token.split('.');
  if (parts.length !== 3) {
    problems.push(`token has ${parts.length} segments, not 3`);
    return problems;
  }
  parts.forEach((p, i) => {
    if (p === '' || !/^[A-Za-z0-9_-]+$/.test(p)) problems.push(`segment ${i} is not base64url`);
  });
  let hdr: { alg?: string; kid?: string };
  try {
    hdr = JSON.parse(TEXT_DECODER.decode(decodeBase64Url(parts[0]))) as { alg?: string; kid?: string };
  } catch {
    problems.push('header is not JSON');
    return problems;
  }
  if (!hdr.alg || hdr.alg === 'none' || hdr.alg.startsWith('HS')) {
    problems.push(`header alg ${hdr.alg ?? '<none>'} is not asymmetric`);
  }
  for (const field of PRIVATE_JWK_FIELDS) {
    if (field in v.jwk) problems.push(`public JWK carries private field ${field}`);
  }
  if (v.jwk.alg !== hdr.alg) problems.push(`JWK alg ${String(v.jwk.alg)} != header alg ${String(hdr.alg)}`);
  if (v.jwk.kid !== hdr.kid) problems.push(`JWK kid ${String(v.jwk.kid)} != header kid ${String(hdr.kid)}`);
  return problems;
}

describe('oauth-jwt cross-language JWT vectors', () => {
  const corpus = loadCorpus();
  const jwtVectors = {
    feature: 'keyring/signing-keyring-contract',
    requirement: 'jwt-vectors-verify-in-both-languages',
    check: 'the-typescript-application-verifies-the-jwt-vectors',
  };

  specTest('the corpus covers both asymmetric families', jwtVectors, () => {
    const families = new Set(corpus.valid.map((v) => v.algorithm));
    expect(families.has('ES256')).toBe(true);
    expect(families.has('RS256')).toBe(true);
  });

  for (const v of corpus.valid) {
    describe(`valid/${v.id}`, () => {
      specTest('passes the structural checks', jwtVectors, () => {
        expect(structuralProblems(v)).toEqual([]);
      });

      specTest('cryptographically verifies the real Go-signed token', jwtVectors, async () => {
        const key = await importPublicKey({ kind: 'jwk', jwk: v.jwk as JsonWebKey & { kid?: string } });
        const payload = await verifyJwt<{ sub?: string; iss?: string }>(v.token, key, {});
        // The pinned payload is the same across every vector.
        expect(payload.sub).toBe('user-123');
        expect(payload.iss).toBe('https://issuer.example');
      });
    });
  }

  for (const v of corpus.invalid) {
    specTest(`invalid/${v.id} fails at least one structural check`, jwtVectors, () => {
      expect(structuralProblems(v).length).toBeGreaterThan(0);
    });
  }
});

describe('oauth-jwt ES256 verification', () => {
  const corpus = loadCorpus();
  const es256 = corpus.valid.find((v) => v.algorithm === 'ES256');
  const rs256 = corpus.valid.find((v) => v.algorithm === 'RS256');

  it('verifyJwtES256 accepts the ES256 vector token', async () => {
    if (!es256) throw new Error('missing ES256 vector');
    const key = await importPublicKey({ kind: 'jwk', jwk: es256.jwk as JsonWebKey & { kid?: string } });
    const payload = await verifyJwtES256<{ sub?: string }>(es256.token, key, {});
    expect(payload.sub).toBe('user-123');
  });

  specTest('verifyJwtES256 rejects a token whose signature has been tampered', forgedToken, async () => {
    if (!es256) throw new Error('missing ES256 vector');
    const key = await importPublicKey({ kind: 'jwk', jwk: es256.jwk as JsonWebKey & { kid?: string } });
    const [h, p, s] = es256.token.split('.');
    // Flip one base64url character of the signature: the token no longer verifies.
    const flipped = (s[0] === 'A' ? 'B' : 'A') + s.slice(1);
    const forged = `${h}.${p}.${flipped}`;
    await expect(verifyJwtES256(forged, key, {})).rejects.toThrow();
  });

  specTest('verifyJwt rejects a symmetric HS256 token (algorithm-confusion defense)', forgedToken, async () => {
    if (!es256) throw new Error('missing ES256 vector');
    const key = await importPublicKey({ kind: 'jwk', jwk: es256.jwk as JsonWebKey & { kid?: string } });
    const hs256 = corpus.invalid.find((v) => v.id === 'symmetric-hs256-alg');
    if (!hs256) throw new Error('missing HS256 vector');
    await expect(verifyJwt(hs256.token, key, {})).rejects.toThrow(/unsupported algorithm/);
  });

  specTest(
    'verifyJwt rejects an ES256 header verified against RS256 material (alg confusion)',
    forgedToken,
    async () => {
      if (!rs256) throw new Error('missing RS256 vector');
      // Import the RSA verify key but present the ES256 token: the dispatcher runs the
      // ECDSA verify against an RSA key, which fails closed.
      const key = await importPublicKey({ kind: 'jwk', jwk: rs256.jwk as JsonWebKey & { kid?: string } });
      if (!es256) throw new Error('missing ES256 vector');
      await expect(verifyJwt(es256.token, key, {})).rejects.toThrow();
    },
  );

  it('verifyJwtRS256 still verifies the RS256 vector (no regression)', async () => {
    if (!rs256) throw new Error('missing RS256 vector');
    const key = await importPublicKey({ kind: 'jwk', jwk: rs256.jwk as JsonWebKey & { kid?: string } });
    const payload = await verifyJwtRS256<{ sub?: string }>(rs256.token, key, {});
    expect(payload.sub).toBe('user-123');
  });
});
