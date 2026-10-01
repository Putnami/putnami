package security

import (
	"crypto"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/json"
	"os"
	"testing"

	"go.putnami.dev/protocol/keyring"
)

// --- Low-level signing primitives are the exact inverse of the verify path ---

// TestSignES256_InverseOfVerifyECDSA signs with signES256 and checks the result
// verifies through the UNCHANGED verifyECDSA (R||S, 32-byte components).
func TestSignES256_InverseOfVerifyECDSA(t *testing.T) {
	key := newECKey(t)
	const signingInput = "eyJhbGciOiJFUzI1NiJ9.eyJzdWIiOiJhIn0"
	sig, err := signES256(signingInput, key)
	if err != nil {
		t.Fatal(err)
	}
	if len(sig) != 2*p256SigComponentLen {
		t.Fatalf("ES256 signature is %d bytes, want %d (R||S fixed-width)", len(sig), 2*p256SigComponentLen)
	}
	if err := verifyECDSA(signingInput, sig, &key.PublicKey, crypto.SHA256, 32); err != nil {
		t.Fatalf("verifyECDSA rejected a signES256 signature: %v", err)
	}
	// A different input must not verify.
	if err := verifyECDSA(signingInput+"x", sig, &key.PublicKey, crypto.SHA256, 32); err == nil {
		t.Fatal("verifyECDSA accepted a signature over a different input")
	}
}

func TestSignRS256_InverseOfVerifyRSA256(t *testing.T) {
	key := parseFixtureRSAKey(t)
	const signingInput = "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJhIn0"
	sig, err := signRS256(signingInput, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyRSA256(signingInput, sig, &key.PublicKey); err != nil {
		t.Fatalf("verifyRSA256 rejected a signRS256 signature: %v", err)
	}
	if err := verifyRSA256(signingInput+"x", sig, &key.PublicKey); err == nil {
		t.Fatal("verifyRSA256 accepted a signature over a different input")
	}
}

// TestSignES256_RejectsNonP256 pins that ES256 signing refuses a non-P-256 key
// rather than emitting an out-of-spec signature width.
func TestSignES256_RejectsNonP256(t *testing.T) {
	// A P-384 key is a valid ECDSA key but the wrong curve for ES256.
	k := generateECKey(t, elliptic.P384())
	if _, err := signES256("a.b", k); err == nil {
		t.Fatal("signES256 accepted a non-P-256 key")
	}
}

func parseFixtureRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	kr := loadKeyringsFixture(t)
	k, err := parseRSAPrivateKey(fixturePrivateJWK(t, kr, "rsa-retiring"))
	if err != nil {
		t.Fatalf("parse rsa fixture: %v", err)
	}
	return k
}

// --- Pre-signed JWT vectors verify through the existing path ---

// TestJWTVectors_ValidTokensVerify checks the offline-signed ES256/RS256 vectors
// from the shared corpus verify through the unchanged validateJWKS path, tying the
// sign side's understanding of the wire format to the shared vectors.
func TestJWTVectors_ValidTokensVerify(t *testing.T) {
	data, err := os.ReadFile(jwtVectorsPath)
	if err != nil {
		t.Fatalf("read jwt-vectors fixture: %v", err)
	}
	var file struct {
		Valid []struct {
			ID        string          `json:"id"`
			Algorithm string          `json:"algorithm"`
			Kid       string          `json:"kid"`
			Token     string          `json:"token"`
			JWK       json.RawMessage `json:"jwk"`
		} `json:"valid"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatalf("parse jwt-vectors: %v", err)
	}
	if len(file.Valid) == 0 {
		t.Fatal("no valid vectors")
	}
	for _, vec := range file.Valid {
		t.Run(vec.ID, func(t *testing.T) {
			var jwk JWK
			if err := json.Unmarshal(vec.JWK, &jwk); err != nil {
				t.Fatalf("parse vector jwk: %v", err)
			}
			fetcher := NewSeededJWKSFetcher("", []JWK{jwk}, 0, false)
			claims, err := validateJWKS(vec.Token, fetcher, "putnami", "https://issuer.example", true)
			if err != nil {
				t.Fatalf("vector %s failed to verify: %v", vec.ID, err)
			}
			if claims.Subject != "user-123" {
				t.Fatalf("vector %s subject = %q", vec.ID, claims.Subject)
			}
		})
	}
}

// TestEC_ProviderReSignsFixtureKey parses the private EC fixture key behind the
// es256 vector and re-signs a fresh token, proving the sign side interoperates
// with the same key material the verify vectors were produced from.
func TestEC_ProviderReSignsFixtureKey(t *testing.T) {
	// The es256 vector's kid is ec-key-1; the same key lives in keyrings.json as
	// ec-active. Give the provider that kid so the JWKS advertises ec-key-1.
	kr := loadKeyringsFixture(t)
	ec, err := parseECPrivateKey(fixturePrivateJWK(t, kr, "ec-active"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewSigningKeyProvider(ProviderConfig{Keys: []SigningKey{
		{Kid: "ec-key-1", Alg: AlgES256, State: keyring.KeyStateActive, EC: ec},
	}})
	if err != nil {
		t.Fatal(err)
	}
	token, err := p.Sign(standardClaims())
	if err != nil {
		t.Fatal(err)
	}
	verifyThroughExistingPath(t, p.PublicJWKS(), token, "putnami", "https://issuer.example")
}
