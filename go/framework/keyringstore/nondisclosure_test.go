package keyringstore

import (
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/protocol/keyring"
)

// The store is the one component that legitimately holds private key material,
// so its own failure paths are exactly where a leak would happen. This test
// hands the store material it cannot process and asserts the resulting error
// carries the key's identity, never the material itself.
func TestStoreErrorsNeverEchoPrivateMaterial(t *testing.T) {
	spectest.Proves(t, "go/signing-key-rotation", "non-disclosure", "a-store-error-reports-key-identity-never-private-material")

	secrets := []string{
		"SECRET-RSA-EXPONENT-d", "SECRET-PRIME-p", "SECRET-PRIME-q", "SECRET-SYMMETRIC-k",
	}
	// Corrupted material rows: each embeds real private parameters inside JSON
	// the store cannot parse, so the unmarshal error is produced while the
	// secret is in hand. Truncation and an invalid token are the two shapes
	// encoding/json reports differently.
	rows := []signingKeyRow{
		{kid: "kid-truncated", material: `{"kty":"RSA","d":"SECRET-RSA-EXPONENT-d","p":"SECRET-PRIME-p"`},
		{kid: "kid-bad-token", material: `{"kty":"oct","k":"SECRET-SYMMETRIC-k","q":"SECRET-PRIME-q",}`},
	}
	for _, row := range rows {
		_, err := jwkFromRow(row)
		if err == nil {
			t.Fatalf("%s: corrupted material unexpectedly parsed", row.kid)
		}
		msg := err.Error()
		if !strings.Contains(msg, row.kid) {
			t.Errorf("%s: error does not identify the key: %q", row.kid, msg)
		}
		for _, secret := range secrets {
			if strings.Contains(msg, secret) {
				t.Errorf("%s: error echoes private material %q: %q", row.kid, secret, msg)
			}
		}
	}

	// The happy-path mapping keeps the material out of everything but the
	// material column itself: kid, state, alg and kty are the only fields a
	// log line or an error would quote.
	jwk := keyring.PrivateJWK{Kid: "kid-ok", Kty: "RSA", Alg: "RS256", D: "SECRET-RSA-EXPONENT-d"}
	row, err := rowFromJWK("kr", jwk)
	if err != nil {
		t.Fatalf("rowFromJWK: %v", err)
	}
	for name, field := range map[string]string{"kid": row.kid, "state": string(row.state), "alg": row.alg, "kty": row.kty} {
		if strings.Contains(field, "SECRET") {
			t.Errorf("identity field %s carries private material: %q", name, field)
		}
	}
}
