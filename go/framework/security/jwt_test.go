package security

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	phttp "go.putnami.dev/http"
	"go.putnami.dev/protocol/features/spectest"
)

const testJWTSecret = "test-secret-key-for-hmac-256"

func makeJWT(t *testing.T, claims map[string]any, secret string) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	payloadEnc := base64.RawURLEncoding.EncodeToString(payload)
	signingInput := header + "." + payloadEnc
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signingInput))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return signingInput + "." + sig
}

func TestJWT_Valid(t *testing.T) {
	token := makeJWT(t, map[string]any{
		"sub":   "service-a",
		"scope": "ingest",
		"exp":   float64(time.Now().Add(time.Hour).Unix()),
	}, testJWTSecret)

	mw := JWT(JWTConfig{Secret: testJWTSecret})

	req := httptest.NewRequest("POST", "/v1/traces", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	ctx := phttp.NewContext(httptest.NewRecorder(), req)

	mw(ctx, func() *phttp.Response { return phttp.JSON("ok") })

	if ctx.User == nil {
		t.Fatal("expected user to be set")
	}
	if ctx.User.Subject != "service-a" {
		t.Errorf("subject = %q, want service-a", ctx.User.Subject)
	}
}

func TestJWT_Expired(t *testing.T) {
	spectest.Proves(t, "go/authentication-authorization", "credential-validation", "jwt-rejects-expired-token")
	token := makeJWT(t, map[string]any{
		"sub": "service-a",
		"exp": float64(time.Now().Add(-time.Hour).Unix()),
	}, testJWTSecret)

	mw := JWT(JWTConfig{Secret: testJWTSecret})

	req := httptest.NewRequest("POST", "/v1/traces", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	ctx := phttp.NewContext(httptest.NewRecorder(), req)

	mw(ctx, func() *phttp.Response { return phttp.JSON("ok") })

	if ctx.User != nil {
		t.Error("expected user to be nil for expired token")
	}
}

func TestJWT_BadSignature(t *testing.T) {
	spectest.Proves(t, "go/authentication-authorization", "credential-validation", "jwt-rejects-bad-signature")
	token := makeJWT(t, map[string]any{"sub": "service-a"}, "wrong-secret")

	mw := JWT(JWTConfig{Secret: testJWTSecret})

	req := httptest.NewRequest("POST", "/v1/traces", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	ctx := phttp.NewContext(httptest.NewRecorder(), req)

	mw(ctx, func() *phttp.Response { return phttp.JSON("ok") })

	if ctx.User != nil {
		t.Error("expected user to be nil for bad signature")
	}
}

func TestJWT_EmptySecretFailsClosed(t *testing.T) {
	// An unset secret (e.g. os.Getenv on a missing var) must not authenticate
	// anyone: an empty HMAC key is not secret, so an attacker can forge a
	// valid signature under it. The middleware must fail closed.
	token := makeJWT(t, map[string]any{
		"sub":   "attacker",
		"roles": "admin",
		"exp":   float64(time.Now().Add(time.Hour).Unix()),
	}, "")

	mw := JWT(JWTConfig{Secret: ""})

	req := httptest.NewRequest("POST", "/v1/traces", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	ctx := phttp.NewContext(httptest.NewRecorder(), req)

	mw(ctx, func() *phttp.Response { return phttp.JSON("ok") })

	if ctx.User != nil {
		t.Error("expected user to be nil: empty secret must fail closed, not accept forged tokens")
	}
}

func TestJWT_MalformedToken(t *testing.T) {
	mw := JWT(JWTConfig{Secret: testJWTSecret})

	req := httptest.NewRequest("POST", "/v1/traces", nil)
	req.Header.Set("Authorization", "Bearer not.a.valid.token")
	ctx := phttp.NewContext(httptest.NewRecorder(), req)

	mw(ctx, func() *phttp.Response { return phttp.JSON("ok") })

	if ctx.User != nil {
		t.Error("expected user to be nil for malformed token")
	}
}

func TestJWT_NoAuthHeader(t *testing.T) {
	mw := JWT(JWTConfig{Secret: testJWTSecret})

	req := httptest.NewRequest("POST", "/v1/traces", nil)
	ctx := phttp.NewContext(httptest.NewRecorder(), req)

	mw(ctx, func() *phttp.Response { return phttp.JSON("ok") })

	if ctx.User != nil {
		t.Error("expected user to be nil when no auth header")
	}
}

func TestJWT_NoExpClaimRejectedByDefault(t *testing.T) {
	token := makeJWT(t, map[string]any{
		"sub":   "service-a",
		"roles": []string{"admin"},
	}, testJWTSecret)

	mw := JWT(JWTConfig{Secret: testJWTSecret})

	req := httptest.NewRequest("POST", "/v1/traces", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	ctx := phttp.NewContext(httptest.NewRecorder(), req)

	mw(ctx, func() *phttp.Response { return phttp.JSON("ok") })

	if ctx.User != nil {
		t.Error("expected user to be nil — a token without exp must be rejected by default")
	}
}

func TestJWT_NoExpClaimAllowed(t *testing.T) {
	token := makeJWT(t, map[string]any{
		"sub":   "service-a",
		"roles": []string{"admin"},
	}, testJWTSecret)

	mw := JWT(JWTConfig{Secret: testJWTSecret, AllowMissingExpiration: true})

	req := httptest.NewRequest("POST", "/v1/traces", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	ctx := phttp.NewContext(httptest.NewRecorder(), req)

	mw(ctx, func() *phttp.Response { return phttp.JSON("ok") })

	if ctx.User == nil {
		t.Fatal("expected user to be set when AllowMissingExpiration is true")
	}
	if !ctx.User.HasRole("admin") {
		t.Error("expected admin role")
	}
}

func TestJWT_AudienceValid(t *testing.T) {
	token := makeJWT(t, map[string]any{
		"sub": "service-a",
		"aud": "billing",
		"exp": float64(time.Now().Add(time.Hour).Unix()),
	}, testJWTSecret)

	mw := JWT(JWTConfig{Secret: testJWTSecret, Audience: "billing"})

	req := httptest.NewRequest("POST", "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	ctx := phttp.NewContext(httptest.NewRecorder(), req)

	mw(ctx, func() *phttp.Response { return phttp.JSON("ok") })

	if ctx.User == nil {
		t.Fatal("expected user to be set for matching audience")
	}
}

func TestJWT_AudienceMismatch(t *testing.T) {
	token := makeJWT(t, map[string]any{
		"sub": "service-a",
		"aud": "other-service",
		"exp": float64(time.Now().Add(time.Hour).Unix()),
	}, testJWTSecret)

	mw := JWT(JWTConfig{Secret: testJWTSecret, Audience: "billing"})

	req := httptest.NewRequest("POST", "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	ctx := phttp.NewContext(httptest.NewRecorder(), req)

	mw(ctx, func() *phttp.Response { return phttp.JSON("ok") })

	if ctx.User != nil {
		t.Error("expected user to be nil for mismatched audience")
	}
}

func TestJWT_IssuerValid(t *testing.T) {
	token := makeJWT(t, map[string]any{
		"sub": "service-a",
		"iss": "https://auth.example.com",
		"exp": float64(time.Now().Add(time.Hour).Unix()),
	}, testJWTSecret)

	mw := JWT(JWTConfig{Secret: testJWTSecret, Issuer: "https://auth.example.com"})

	req := httptest.NewRequest("POST", "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	ctx := phttp.NewContext(httptest.NewRecorder(), req)

	mw(ctx, func() *phttp.Response { return phttp.JSON("ok") })

	if ctx.User == nil {
		t.Fatal("expected user to be set for matching issuer")
	}
}

func TestJWT_IssuerMismatch(t *testing.T) {
	token := makeJWT(t, map[string]any{
		"sub": "service-a",
		"iss": "https://evil.example.com",
		"exp": float64(time.Now().Add(time.Hour).Unix()),
	}, testJWTSecret)

	mw := JWT(JWTConfig{Secret: testJWTSecret, Issuer: "https://auth.example.com"})

	req := httptest.NewRequest("POST", "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	ctx := phttp.NewContext(httptest.NewRecorder(), req)

	mw(ctx, func() *phttp.Response { return phttp.JSON("ok") })

	if ctx.User != nil {
		t.Error("expected user to be nil for mismatched issuer")
	}
}

func TestJWT_ClaimsMapped(t *testing.T) {
	token := makeJWT(t, map[string]any{
		"sub":       "user-1",
		"iss":       "https://auth.example.com",
		"client_id": "my-app",
		"scope":     "read write",
		"roles":     []string{"editor"},
		"exp":       float64(time.Now().Add(time.Hour).Unix()),
	}, testJWTSecret)

	mw := JWT(JWTConfig{Secret: testJWTSecret})

	req := httptest.NewRequest("POST", "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	ctx := phttp.NewContext(httptest.NewRecorder(), req)

	mw(ctx, func() *phttp.Response { return phttp.JSON("ok") })

	if ctx.User == nil {
		t.Fatal("expected user to be set")
	}
	if ctx.User.Subject != "user-1" {
		t.Errorf("subject = %q, want user-1", ctx.User.Subject)
	}
	if ctx.User.Issuer != "https://auth.example.com" {
		t.Errorf("issuer = %q, want https://auth.example.com", ctx.User.Issuer)
	}
	if ctx.User.ClientID != "my-app" {
		t.Errorf("client_id = %q, want my-app", ctx.User.ClientID)
	}
	if !ctx.User.HasScope("read") || !ctx.User.HasScope("write") {
		t.Errorf("scopes = %v, want [read write]", ctx.User.Scopes)
	}
	if !ctx.User.HasRole("editor") {
		t.Errorf("roles = %v, want [editor]", ctx.User.Roles)
	}
}
