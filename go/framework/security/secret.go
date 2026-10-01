package security

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"io"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/keyring"
)

// This file implements versioned secret digests on top of the shared PHC-style
// grammar defined in go.putnami.dev/protocol/keyring. A digest produced by
// HashSecret is exactly a keyring.Digest.String(): it parses clean through
// keyring.ParseAndValidateDigest and round-trips byte-for-byte, so the on-disk
// form is the single cross-language contract that the TypeScript implementation
// must reproduce. No secret or key material is ever logged from this package.

// Security policy constants for pbkdf2-sha256 password digests. These are a
// deployment POLICY layered on top of the keyring GRAMMAR bounds
// (keyring.MinIterations / keyring.MaxIterations, which merely bound a
// well-formed string). The floor here is what a caller may not go below.
const (
	// DefaultPBKDF2Iterations is the current pbkdf2-sha256 work-factor preset
	// applied when a caller does not tune it. It follows the OWASP 2023 guidance
	// for PBKDF2-HMAC-SHA256 (600k). A stored digest below this preset triggers a
	// rehash on the next successful verify so callers upgrade transparently.
	DefaultPBKDF2Iterations = 600_000

	// IterationFloor is the security floor for the tunable pbkdf2 work factor:
	// WithIterations can never configure a value below this, no matter what the
	// caller passes. It sits well above keyring.MinIterations (a grammar bound of
	// 1, not a policy) so a mis-tuned deployment can never fall to an unsafe work
	// factor.
	IterationFloor = 210_000

	// DefaultSaltLength is the random salt length in bytes. 16 bytes (128 bits)
	// exceeds the RFC 8018 minimum and matches common PHC practice.
	DefaultSaltLength = 16

	// hashLength is the derived-key / HMAC output length in bytes. One SHA-256
	// block; fixed so digests are a stable width across languages.
	hashLength = sha256.Size
)

// VerifyResult is the outcome of VerifySecret. Valid reports whether the secret
// matched the stored digest (compared in constant time). RehashRequired reports
// whether the stored digest's parameters are weaker than the current preset, so
// a caller that just verified successfully can transparently re-hash and store
// an upgraded digest. RehashRequired is meaningful only when Valid is true; a
// failed verify always returns RehashRequired=false.
type VerifyResult struct {
	Valid          bool
	RehashRequired bool
}

// hashConfig holds the tunable parameters for HashSecret plus the salt source
// seam. Its zero value is never used directly; defaultHashConfig seeds the
// production defaults.
type hashConfig struct {
	algorithm  keyring.DigestAlgorithm
	iterations int
	saltLen    int
	// rand is the salt source. Production uses crypto/rand.Reader; the internal
	// withRand seam pins it in tests so deterministic cross-language vectors can
	// be computed. It is never exposed to callers, so a non-random salt cannot be
	// injected in production.
	rand io.Reader
}

func defaultHashConfig() hashConfig {
	return hashConfig{
		algorithm:  keyring.DigestPBKDF2SHA256,
		iterations: DefaultPBKDF2Iterations,
		saltLen:    DefaultSaltLength,
		rand:       rand.Reader,
	}
}

// Option tunes HashSecret. Options are applied in order over the defaults.
type Option func(*hashConfig)

// WithAlgorithm selects the digest algorithm. Use keyring.DigestPBKDF2SHA256
// (the default) for low-entropy secrets such as passwords, and
// keyring.DigestHMACSHA256 for high-entropy secrets such as randomly generated
// API tokens, where a work factor buys nothing. An unrecognized algorithm makes
// HashSecret return an error.
func WithAlgorithm(alg keyring.DigestAlgorithm) Option {
	return func(c *hashConfig) { c.algorithm = alg }
}

// WithIterations tunes the pbkdf2-sha256 work factor. The value is CLAMPED into
// [IterationFloor, keyring.MaxIterations]: a caller can raise the cost but can
// never drop below the security floor, so a mis-configuration cannot weaken a
// password digest. It has no effect on hmac-sha256 (which carries no work
// factor).
func WithIterations(n int) Option {
	return func(c *hashConfig) {
		if n < IterationFloor {
			n = IterationFloor
		}
		if n > keyring.MaxIterations {
			n = keyring.MaxIterations
		}
		c.iterations = n
	}
}

// withRand overrides the salt source. It is the internal salt seam: unexported
// so only in-package test code (which computes the pinned cross-language
// vectors) can supply a deterministic salt. Production callers cannot reach it,
// so HashSecret always draws a fresh random salt from crypto/rand.
func withRand(r io.Reader) Option {
	return func(c *hashConfig) { c.rand = r }
}

// HashSecret derives a versioned PHC-style digest for secret and returns its
// canonical keyring wire form (e.g. $pbkdf2-sha256$v=1$i=600000$<salt>$<hash>).
// A fresh random salt is drawn from crypto/rand for every call, so two calls on
// the same secret produce different digests; verification recovers the salt from
// the stored digest. The returned string always parses clean through
// keyring.ParseAndValidateDigest. The secret is never logged.
func HashSecret(secret string, opts ...Option) (string, error) {
	cfg := defaultHashConfig()
	for _, o := range opts {
		o(&cfg)
	}
	if !cfg.algorithm.Valid() {
		return "", fmt.Errorf("security: unsupported digest algorithm %q", cfg.algorithm)
	}
	salt := make([]byte, cfg.saltLen)
	if _, err := io.ReadFull(cfg.rand, salt); err != nil {
		return "", fmt.Errorf("security: read salt: %w", err)
	}
	return encodeDigest(cfg.algorithm, secret, salt, cfg.iterations)
}

// encodeDigest computes the hash for the given algorithm/salt/iterations and
// encodes the result through keyring.Digest.String, validating it first so a
// malformed digest can never be emitted. Iterations is stamped only for
// pbkdf2-sha256 (hmac-sha256 carries no work factor, and keyring.ValidateDigest
// rejects a non-zero count there).
func encodeDigest(alg keyring.DigestAlgorithm, secret string, salt []byte, iterations int) (string, error) {
	hash, err := computeHash(alg, secret, salt, iterations)
	if err != nil {
		return "", err
	}
	d := keyring.Digest{
		Algorithm: alg,
		Version:   keyring.DigestVersion,
		Salt:      salt,
		Hash:      hash,
	}
	if alg == keyring.DigestPBKDF2SHA256 {
		d.Iterations = iterations
	}
	if diags := keyring.ValidateDigest(&d); diag.HasErrors(diags) {
		return "", fmt.Errorf("security: constructed digest failed validation: %v", diags)
	}
	return d.String(), nil
}

// computeHash derives the raw digest bytes for a secret under a given algorithm,
// salt and iteration count. It is the deterministic core shared by HashSecret
// and VerifySecret: identical (algorithm, secret, salt, iterations) inputs
// always yield identical bytes, which is what makes the cross-language vectors
// reproducible. Only the salt varies between HashSecret calls.
func computeHash(alg keyring.DigestAlgorithm, secret string, salt []byte, iterations int) ([]byte, error) {
	switch alg {
	case keyring.DigestPBKDF2SHA256:
		return pbkdf2.Key(sha256.New, secret, salt, iterations, hashLength)
	case keyring.DigestHMACSHA256:
		// Salted keyed HMAC: key = salt, message = secret. A single pass is
		// sufficient for high-entropy secrets, and keying by the stored salt makes
		// each digest unique even for identical tokens.
		mac := hmac.New(sha256.New, salt)
		mac.Write([]byte(secret))
		return mac.Sum(nil), nil
	default:
		return nil, fmt.Errorf("security: unsupported digest algorithm %q", alg)
	}
}

// VerifySecret checks secret against a stored PHC digest string. It parses and
// validates the digest through the shared keyring grammar, recomputes the hash
// with the digest's own salt/iterations, and compares in CONSTANT TIME via
// crypto/subtle.ConstantTimeCompare. A digest that fails to parse or validate,
// or a mismatched secret, returns {Valid:false}. On a successful match,
// RehashRequired reports whether the stored digest is weaker than the current
// preset (see needsRehash) so the caller can upgrade it transparently. The
// secret is never logged.
func VerifySecret(secret, digestString string) VerifyResult {
	d, diags := keyring.ParseAndValidateDigest(digestString)
	if d == nil || diag.HasErrors(diags) {
		return VerifyResult{Valid: false}
	}
	computed, err := computeHash(d.Algorithm, secret, d.Salt, d.Iterations)
	if err != nil {
		return VerifyResult{Valid: false}
	}
	if subtle.ConstantTimeCompare(computed, d.Hash) != 1 {
		return VerifyResult{Valid: false}
	}
	return VerifyResult{Valid: true, RehashRequired: needsRehash(d)}
}

// needsRehash reports whether a validated stored digest should be re-hashed
// against the current preset after a successful verify. For pbkdf2-sha256 that
// is an iteration count below DefaultPBKDF2Iterations (the caller raised the
// cost since the digest was written). hmac-sha256 carries no work factor, so it
// never needs a rehash on its own. This is the single place to extend when a new
// preset or a deprecated algorithm is introduced.
func needsRehash(d *keyring.Digest) bool {
	switch d.Algorithm {
	case keyring.DigestPBKDF2SHA256:
		return d.Iterations < DefaultPBKDF2Iterations
	default:
		return false
	}
}
