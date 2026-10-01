package security

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	phttp "go.putnami.dev/http"
)

// TestRollover_OverlapKeepsInFlightTokensValid_RevocationFailsClosed is the
// dogfood proof: a single readable narrative that exercises the whole rollover
// contract through the package's Sign/Rotate/Revoke state machine and verifies
// every token through the REAL public verify path (see rotationProofVerify).
//
//	K1 active                → T1 (signed by K1) verifies.
//	Rotate in K2 (K1 → retiring, overlap):
//	  - JWKS publishes BOTH K1 and K2 (overlap window).
//	  - T1 (retiring K1) STILL verifies — no in-flight token invalidated.
//	  - T2 (signed by K2) verifies.
//	Revoke K1 (already retiring after the rotation):
//	  - K1's kid LEAVES the JWKS.
//	  - T1 NO LONGER verifies (revoked kid, fail-closed).
//	  - T2 still verifies.
func TestRollover_OverlapKeepsInFlightTokensValid_RevocationFailsClosed(t *testing.T) {
	// A signing provider with a single active key K1. A generous overlap keeps
	// the retiring predecessor published across the rollover; the window itself
	// is exercised by state changes, not by advancing any clock.
	p, err := newProvider(ProviderConfig{
		OverlapWindow: time.Hour,
		Keys:          []SigningKey{{Kid: "K1", EC: newECKey(t)}},
	})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}

	// T1 signed under K1 verifies through the real verify path against the JWKS.
	t1 := rotationProofSign(t, p)
	rotationProofAssertVerifies(t, p, t1, "T1 (active K1) before any rotation")
	rotationProofAssertKIDs(t, p, map[string]bool{"K1": true})

	// --- Rotate in K2: K1 → retiring, K2 → active (overlap window open) ---
	if err := p.Rotate(SigningKey{Kid: "K2", EC: newECKey(t)}); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if p.ActiveKID() != "K2" {
		t.Fatalf("after rotate active kid = %q, want K2", p.ActiveKID())
	}
	// Both keys are published during the overlap so a verifier that refetched the
	// rotated JWKS finds K2 AND still trusts K1.
	rotationProofAssertKIDs(t, p, map[string]bool{"K1": true, "K2": true})
	// T1 was signed by the now-retiring K1 and MUST still verify during overlap.
	rotationProofAssertVerifies(t, p, t1, "T1 (retiring K1) during the overlap window")
	// A fresh token signs under the new active K2 and verifies.
	t2 := rotationProofSign(t, p)
	rotationProofAssertVerifies(t, p, t2, "T2 (active K2) after rotation")

	// --- Revoke K1: it is already retiring after the rotation, so Revoke is legal ---
	if err := p.Revoke("K1"); err != nil {
		t.Fatalf("revoke retiring K1: %v", err)
	}
	// K1 leaves the JWKS (fail-closed publication); K2 remains.
	rotationProofAssertKIDs(t, p, map[string]bool{"K2": true})
	// T1's kid is gone, so T1 no longer verifies — a revoked key's tokens fail closed.
	rotationProofAssertRejects(t, p, t1, "T1 (revoked K1) after revocation")
	// T2 under the still-active K2 keeps verifying.
	rotationProofAssertVerifies(t, p, t2, "T2 (active K2) after K1's revocation")

	// The active key cannot be revoked directly — that would leave no active key.
	if err := p.Revoke("K2"); err == nil {
		t.Fatal("expected error revoking the active key K2 (would leave zero active keys)")
	}
}

// --- helpers ---

const (
	rotationProofAudience = "putnami"
	rotationProofIssuer   = "https://issuer.example"
)

func rotationProofClaims() map[string]any {
	return map[string]any{
		"sub": "user-123",
		"iss": rotationProofIssuer,
		"aud": rotationProofAudience,
		"exp": 4102444800, // year 2100
		"iat": 1700000000,
	}
}

func rotationProofSign(t *testing.T, p SigningKeyProvider) string {
	t.Helper()
	token, err := p.Sign(rotationProofClaims())
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return token
}

// rotationProofVerify reports whether token verifies against p's published JWKS
// through the REAL, PUBLIC Putnami verify path. It parses the published JWKS
// with ParseJWKS, seeds it into a JWKSJWT identity resolver, and drives token
// through that resolver as a bearer credential. A resolved (non-nil) identity
// means the JWS signature verified AND the signing kid is currently published;
// a nil identity means the token was rejected — an unknown or revoked kid, or a
// bad signature.
func rotationProofVerify(p SigningKeyProvider, token string) (bool, error) {
	raw, err := json.Marshal(p.PublicJWKS())
	if err != nil {
		return false, fmt.Errorf("marshal published JWKS: %w", err)
	}
	parsed, err := ParseJWKS(string(raw))
	if err != nil {
		return false, fmt.Errorf("parse published JWKS: %w", err)
	}
	resolver := JWKSJWT(JWKSJWTConfig{
		SeedKeys:       parsed.Keys,
		Audience:       rotationProofAudience,
		RequiredIssuer: rotationProofIssuer,
	})
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	ctx := phttp.NewContext(httptest.NewRecorder(), req)
	resolver(ctx, func() *phttp.Response { return phttp.JSON("ok") })
	return ctx.User != nil, nil
}

func rotationProofAssertVerifies(t *testing.T, p SigningKeyProvider, token, what string) {
	t.Helper()
	ok, err := rotationProofVerify(p, token)
	if err != nil {
		t.Fatalf("verify %s: %v", what, err)
	}
	if !ok {
		t.Fatalf("%s: expected the token to verify, but it was rejected", what)
	}
}

func rotationProofAssertRejects(t *testing.T, p SigningKeyProvider, token, what string) {
	t.Helper()
	ok, err := rotationProofVerify(p, token)
	if err != nil {
		t.Fatalf("verify %s: %v", what, err)
	}
	if ok {
		t.Fatalf("%s: expected the token to be REJECTED, but it verified", what)
	}
}

func rotationProofAssertKIDs(t *testing.T, p SigningKeyProvider, want map[string]bool) {
	t.Helper()
	got := make(map[string]bool, len(p.PublicJWKS().Keys))
	for _, k := range p.PublicJWKS().Keys {
		got[k.Kid] = true
	}
	if len(got) != len(want) {
		t.Fatalf("published kids = %v, want %v", got, want)
	}
	for kid := range want {
		if !got[kid] {
			t.Fatalf("published kids = %v, missing %q", got, kid)
		}
	}
}
