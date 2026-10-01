import { describe, expect, it } from 'bun:test';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import {
  DEFAULT_PBKDF2_ITERATIONS,
  type DigestAlgorithm,
  hashSecret,
  ITERATION_FLOOR,
  parseDigest,
  verifySecret,
  _hashWithSalt,
} from '../../src/security/secret-digest';

// The shared, language-neutral fixture the Go implementation
// (go/framework/security/secret_test.go) also drives. Byte-for-byte parity with
// its pinned digests is the core cross-language contract of this slice.
const SECRET_VECTORS_PATH = join(__dirname, '../../../../../protocols/keyring/fixtures/secret-vectors.json');

interface SecretVector {
  id: string;
  algorithm: DigestAlgorithm;
  secret: string;
  salt: string;
  iterations: number;
  digest: string;
  rehashRequired: boolean;
  valid: string[];
  invalid: string[];
}

function loadSecretVectors(): SecretVector[] {
  const doc = JSON.parse(readFileSync(SECRET_VECTORS_PATH, 'utf8')) as { vectors: SecretVector[] };
  expect(doc.vectors.length).toBeGreaterThan(0);
  return doc.vectors;
}

// Decodes a vector's standard-base64 (no padding) salt to its raw bytes. Node's
// Buffer accepts unpadded standard base64, matching Go's base64.RawStdEncoding
// for the canonical fixtures.
function decodeSalt(b64: string): Uint8Array {
  return new Uint8Array(Buffer.from(b64, 'base64'));
}

describe('secret-digest cross-language vectors', () => {
  const vectors = loadSecretVectors();

  it('covers both digest algorithms', () => {
    const families = new Set(vectors.map((v) => v.algorithm));
    expect(families.has('pbkdf2-sha256')).toBe(true);
    expect(families.has('hmac-sha256')).toBe(true);
  });

  for (const v of vectors) {
    describe(v.id, () => {
      it('pinned digest parses clean through the keyring grammar', () => {
        const parsed = parseDigest(v.digest);
        expect(parsed).toBeDefined();
        expect(parsed?.algorithm).toBe(v.algorithm);
      });

      it('every valid secret verifies true with the pinned rehashRequired flag', async () => {
        for (const secret of v.valid) {
          const res = await verifySecret(secret, v.digest);
          expect(res.valid).toBe(true);
          expect(res.rehashRequired).toBe(v.rehashRequired);
        }
      });

      it('every invalid secret fails closed', async () => {
        for (const secret of v.invalid) {
          const res = await verifySecret(secret, v.digest);
          expect(res.valid).toBe(false);
          expect(res.rehashRequired).toBe(false);
        }
      });

      it('re-hashing the secret with the pinned salt reproduces the EXACT digest (byte-identical to Go)', async () => {
        const salt = decodeSalt(v.salt);
        const opts =
          v.algorithm === 'pbkdf2-sha256'
            ? { algorithm: v.algorithm, iterations: v.iterations }
            : { algorithm: v.algorithm };
        const got = await _hashWithSalt(v.secret, salt, opts);
        expect(got).toBe(v.digest);
      });
    });
  }
});

describe('secret-digest round-trip', () => {
  const algorithms: DigestAlgorithm[] = ['pbkdf2-sha256', 'hmac-sha256'];

  // These cases hash at the real default work factor (600k PBKDF2 iterations),
  // several times per case. That is the point — a round trip at a reduced cost
  // would not exercise the shipped parameters — but it also means the default
  // per-test budget is not the right one: on a machine running the rest of the
  // suite in parallel these are CPU-bound for seconds. State the real budget
  // rather than let contention decide whether the suite is green.
  const ROUND_TRIP_TIMEOUT_MS = 60_000;

  for (const algorithm of algorithms) {
    describe(algorithm, () => {
      // Includes multibyte runes to pin the UTF-8 secret encoding.
      const secret = 's3cr3t-éà-value';

      it(
        'a fresh digest verifies for the correct secret and fails for a wrong one',
        async () => {
          const digest = await hashSecret(secret, { algorithm });
          expect(parseDigest(digest)).toBeDefined();

          const ok = await verifySecret(secret, digest);
          expect(ok.valid).toBe(true);

          const wrong = await verifySecret(`${secret}x`, digest);
          expect(wrong.valid).toBe(false);
          expect(wrong.rehashRequired).toBe(false);
        },
        ROUND_TRIP_TIMEOUT_MS,
      );

      it(
        'two calls draw independent random salts and yield distinct digests',
        async () => {
          const a = await hashSecret(secret, { algorithm });
          const b = await hashSecret(secret, { algorithm });
          expect(a).not.toBe(b);
          expect((await verifySecret(secret, a)).valid).toBe(true);
          expect((await verifySecret(secret, b)).valid).toBe(true);
        },
        ROUND_TRIP_TIMEOUT_MS,
      );
    });
  }
});

describe('secret-digest policy', () => {
  const secret = 'correct horse';

  it('a below-preset pbkdf2 digest reports rehashRequired on a successful verify', async () => {
    const weak = await _hashWithSalt(secret, decodeSalt('MDEyMzQ1Njc4OWFiY2RlZg'), {
      algorithm: 'pbkdf2-sha256',
      iterations: ITERATION_FLOOR,
    });
    const res = await verifySecret(secret, weak);
    expect(res.valid).toBe(true);
    expect(res.rehashRequired).toBe(true);
  });

  // Hashes and verifies at the shipped 600k-iteration preset, so it carries the
  // same explicit budget as the round-trip cases above rather than the default.
  it('a preset pbkdf2 digest does not require a rehash', async () => {
    const strong = await hashSecret(secret); // default preset
    const res = await verifySecret(secret, strong);
    expect(res.valid).toBe(true);
    expect(res.rehashRequired).toBe(false);
  }, 60_000);

  it('the iteration floor cannot be undercut', async () => {
    const floored = await hashSecret(secret, { iterations: 1 });
    const parsed = parseDigest(floored);
    expect(parsed?.iterations).toBe(ITERATION_FLOOR);
    // The floor must sit below the preset so there is tuning room above it.
    expect(ITERATION_FLOOR).toBeLessThan(DEFAULT_PBKDF2_ITERATIONS);
  });
});

describe('secret-digest fail-closed parsing', () => {
  const malformed = [
    '',
    'not-a-digest',
    '$unknown$v=1$AAAA$BBBB',
    '$pbkdf2-sha256$v=2$i=1$AAAA$BBBB', // unsupported version
    '$pbkdf2-sha256$v=1$i=0$AAAA$BBBB', // iterations below grammar floor
    '$hmac-sha256$v=1$AAAA', // too few segments
    '$pbkdf2-sha256$v=1$i=1$@@@@$BBBB', // non-base64 salt
    '$pbkdf2-sha256$v=1$i=1$AA-_$BBBB', // base64url alphabet is rejected (must be standard)
  ];

  it('parseDigest rejects malformed strings', () => {
    for (const bad of malformed) {
      expect(parseDigest(bad)).toBeUndefined();
    }
  });

  it('verifySecret fails closed on a malformed or empty stored digest', async () => {
    for (const bad of malformed) {
      const res = await verifySecret('whatever', bad);
      expect(res.valid).toBe(false);
      expect(res.rehashRequired).toBe(false);
    }
  });
});
