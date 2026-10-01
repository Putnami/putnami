package security

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/protocol/keyring"
)

// secretVectorsPath is the shared, language-neutral fixture the TypeScript
// implementation must reproduce byte-for-byte. The protocol module owns the
// cross-language corpus.
const secretVectorsPath = "../../../protocols/keyring/fixtures/secret-vectors.json"

type secretVector struct {
	ID             string   `json:"id"`
	Algorithm      string   `json:"algorithm"`
	Secret         string   `json:"secret"`
	Salt           string   `json:"salt"`
	Iterations     int      `json:"iterations"`
	Digest         string   `json:"digest"`
	RehashRequired bool     `json:"rehashRequired"`
	Valid          []string `json:"valid"`
	Invalid        []string `json:"invalid"`
}

func loadSecretVectors(t *testing.T) []secretVector {
	t.Helper()
	data, err := os.ReadFile(secretVectorsPath)
	if err != nil {
		t.Fatalf("read %s: %v", secretVectorsPath, err)
	}
	var doc struct {
		Vectors []secretVector `json:"vectors"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decode %s: %v", secretVectorsPath, err)
	}
	if len(doc.Vectors) == 0 {
		t.Fatalf("%s has no vectors", secretVectorsPath)
	}
	return doc.Vectors
}

// TestSecretVectors_CrossLanguage is requirement (a): every shared vector drives
// VerifySecret. The valid secret verifies with the pinned RehashRequired flag,
// every invalid secret fails closed, the stored digest parses clean through the
// keyring grammar, and re-hashing with the pinned salt reproduces the EXACT
// digest string — the byte-for-byte contract the TypeScript twin must match.
func TestSecretVectors_CrossLanguage(t *testing.T) {
	vectors := loadSecretVectors(t)

	// Require both algorithms to be represented so the corpus can't silently drop
	// a family.
	families := map[string]bool{}
	for _, v := range vectors {
		families[v.Algorithm] = true
	}
	for _, want := range []string{string(keyring.DigestPBKDF2SHA256), string(keyring.DigestHMACSHA256)} {
		if !families[want] {
			t.Errorf("secret vectors missing algorithm %q", want)
		}
	}

	for _, v := range vectors {
		t.Run(v.ID, func(t *testing.T) {
			// (c) the pinned digest must parse+validate clean through the shared grammar.
			if _, diags := keyring.ParseAndValidateDigest(v.Digest); diag.HasErrors(diags) {
				t.Fatalf("pinned digest does not parse clean: %v", diags)
			}

			for _, secret := range v.Valid {
				res := VerifySecret(secret, v.Digest)
				if !res.Valid {
					t.Errorf("valid secret failed to verify against %s", v.ID)
				}
				if res.RehashRequired != v.RehashRequired {
					t.Errorf("RehashRequired = %t, want %t", res.RehashRequired, v.RehashRequired)
				}
			}
			for _, secret := range v.Invalid {
				res := VerifySecret(secret, v.Digest)
				if res.Valid {
					t.Errorf("invalid secret %q was accepted against %s", secret, v.ID)
				}
			}

			// Deterministic reproduction: re-hashing the vector's secret with the
			// pinned salt must yield the identical digest string.
			salt, err := base64.RawStdEncoding.DecodeString(v.Salt)
			if err != nil {
				t.Fatalf("decode salt: %v", err)
			}
			opts := []Option{WithAlgorithm(keyring.DigestAlgorithm(v.Algorithm)), withRand(bytes.NewReader(salt))}
			if v.Algorithm == string(keyring.DigestPBKDF2SHA256) {
				opts = append(opts, WithIterations(v.Iterations))
			}
			got, err := HashSecret(v.Secret, opts...)
			if err != nil {
				t.Fatalf("HashSecret: %v", err)
			}
			if got != v.Digest {
				t.Fatalf("re-hash mismatch\n got:  %s\n want: %s", got, v.Digest)
			}
		})
	}
}

// TestHashVerify_RoundTrip is requirements (b) and (c): a fresh HashSecret digest
// (random salt) verifies for the correct secret, fails for a wrong one, and every
// produced digest parses clean through keyring.ParseAndValidateDigest.
func TestHashVerify_RoundTrip(t *testing.T) {
	spectest.Proves(t, "go/authentication-authorization", "password-secret-policy", "versioned-hash-round-trip")
	algorithms := []keyring.DigestAlgorithm{keyring.DigestPBKDF2SHA256, keyring.DigestHMACSHA256}
	for _, alg := range algorithms {
		t.Run(string(alg), func(t *testing.T) {
			const secret = "s3cr3t-éà-value" // includes multibyte runes
			digest, err := HashSecret(secret, WithAlgorithm(alg))
			if err != nil {
				t.Fatalf("HashSecret: %v", err)
			}
			if _, diags := keyring.ParseAndValidateDigest(digest); diag.HasErrors(diags) {
				t.Fatalf("HashSecret output does not parse clean: %v (%s)", diags, digest)
			}
			if res := VerifySecret(secret, digest); !res.Valid {
				t.Errorf("correct secret failed to verify")
			}
			if res := VerifySecret(secret+"x", digest); res.Valid {
				t.Errorf("wrong secret verified")
			}

			// Two calls draw independent random salts → distinct digests.
			other, err := HashSecret(secret, WithAlgorithm(alg))
			if err != nil {
				t.Fatalf("HashSecret: %v", err)
			}
			if other == digest {
				t.Errorf("expected a fresh random salt to yield a different digest")
			}
			if res := VerifySecret(secret, other); !res.Valid {
				t.Errorf("correct secret failed to verify against second digest")
			}
		})
	}
}

// TestVerify_RejectsMalformed asserts a non-parseable or empty stored digest
// fails closed rather than panicking or accepting.
func TestVerify_RejectsMalformed(t *testing.T) {
	spectest.Proves(t, "go/authentication-authorization", "password-secret-policy", "malformed-hash-fails-closed")
	for _, bad := range []string{"", "not-a-digest", "$unknown$v=1$AAAA$BBBB", "$pbkdf2-sha256$v=2$i=1$AAAA$BBBB"} {
		if res := VerifySecret("whatever", bad); res.Valid {
			t.Errorf("malformed digest %q was accepted", bad)
		}
	}
}

// TestConstantTimeComparison is requirement (d): a structural guard that
// VerifySecret compares the recomputed hash with crypto/subtle's constant-time
// primitive and does NOT fall back to a variable-time byte comparison. This pins
// the timing-safety invariant against a future refactor.
func TestConstantTimeComparison(t *testing.T) {
	spectest.Proves(t, "go/authentication-authorization", "password-secret-policy", "secret-comparison-is-constant-time")
	src, err := os.ReadFile("secret.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	if !strings.Contains(s, "subtle.ConstantTimeCompare") {
		t.Error("VerifySecret must compare digests with subtle.ConstantTimeCompare")
	}
	if !strings.Contains(s, `"crypto/subtle"`) {
		t.Error("secret.go must import crypto/subtle")
	}
	for _, forbidden := range []string{"bytes.Equal(", "== d.Hash", "d.Hash =="} {
		if strings.Contains(s, forbidden) {
			t.Errorf("secret.go uses a variable-time comparison %q; use subtle.ConstantTimeCompare", forbidden)
		}
	}
}

// TestRehashRequired is requirement (e): a digest hashed below the current preset
// reports RehashRequired on a successful verify, one at the preset does not, and
// WithIterations clamps a caller below the floor up to IterationFloor (it can
// never weaken the work factor).
func TestRehashRequired(t *testing.T) {
	spectest.Proves(t, "go/authentication-authorization", "password-secret-policy", "rehash-policy-is-explicit")
	const secret = "correct horse"

	weak, err := HashSecret(secret, WithIterations(IterationFloor))
	if err != nil {
		t.Fatal(err)
	}
	if res := VerifySecret(secret, weak); !res.Valid || !res.RehashRequired {
		t.Errorf("below-preset digest: Valid=%t RehashRequired=%t, want true/true", res.Valid, res.RehashRequired)
	}

	strong, err := HashSecret(secret) // default preset
	if err != nil {
		t.Fatal(err)
	}
	if res := VerifySecret(secret, strong); !res.Valid || res.RehashRequired {
		t.Errorf("preset digest: Valid=%t RehashRequired=%t, want true/false", res.Valid, res.RehashRequired)
	}

	// A failed verify never claims RehashRequired.
	if res := VerifySecret("wrong", weak); res.Valid || res.RehashRequired {
		t.Errorf("failed verify: Valid=%t RehashRequired=%t, want false/false", res.Valid, res.RehashRequired)
	}

	// The floor cannot be undercut: asking for 1 iteration yields a digest pinned
	// at IterationFloor, not below it.
	floored, err := HashSecret(secret, WithIterations(1))
	if err != nil {
		t.Fatal(err)
	}
	d, diags := keyring.ParseAndValidateDigest(floored)
	if diag.HasErrors(diags) {
		t.Fatalf("floored digest invalid: %v", diags)
	}
	if d.Iterations != IterationFloor {
		t.Errorf("WithIterations(1) produced i=%d, want the floor %d", d.Iterations, IterationFloor)
	}
	if IterationFloor >= DefaultPBKDF2Iterations {
		t.Errorf("floor %d must sit below the preset %d to leave tuning room", IterationFloor, DefaultPBKDF2Iterations)
	}
}
