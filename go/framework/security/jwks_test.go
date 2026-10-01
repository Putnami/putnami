package security

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	phttp "go.putnami.dev/http"
	"go.putnami.dev/protocol/features/spectest"
)

// --- Test helpers ---

func generateRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func rsaJWKJSON(kid string, pub *rsa.PublicKey) map[string]any {
	return map[string]any{
		"kty": "RSA",
		"kid": kid,
		"use": "sig",
		"alg": "RS256",
		"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}
}

func serveJWKS(t *testing.T, keys ...map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"keys": keys})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func makeRSAJWT(t *testing.T, claims map[string]any, kid string, privKey *rsa.PrivateKey) string { //nolint:unparam // kid varies in future tests
	t.Helper()
	headerJSON, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": kid})
	header := base64.RawURLEncoding.EncodeToString(headerJSON)
	payloadJSON, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	payload := base64.RawURLEncoding.EncodeToString(payloadJSON)
	signingInput := header + "." + payload

	h := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, privKey, crypto.SHA256, h[:])
	if err != nil {
		t.Fatal(err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func generateECKey(t *testing.T, curve elliptic.Curve) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(curve, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// leftPad left-pads b with zero bytes to exactly size bytes (JWS R||S encoding).
func leftPad(b []byte, size int) []byte {
	if len(b) >= size {
		return b
	}
	out := make([]byte, size)
	copy(out[size-len(b):], b)
	return out
}

func ecJWKJSON(kid, alg string, pub *ecdsa.PublicKey) map[string]any {
	crv := "P-256"
	size := 32
	if pub.Curve == elliptic.P384() {
		crv = "P-384"
		size = 48
	}
	return map[string]any{
		"kty": "EC",
		"kid": kid,
		"use": "sig",
		"alg": alg,
		"crv": crv,
		"x":   base64.RawURLEncoding.EncodeToString(leftPad(pub.X.Bytes(), size)),
		"y":   base64.RawURLEncoding.EncodeToString(leftPad(pub.Y.Bytes(), size)),
	}
}

func makeECJWT(t *testing.T, claims map[string]any, kid string, priv *ecdsa.PrivateKey, alg string) string {
	t.Helper()
	headerJSON, _ := json.Marshal(map[string]string{"alg": alg, "typ": "JWT", "kid": kid})
	header := base64.RawURLEncoding.EncodeToString(headerJSON)
	payloadJSON, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	payload := base64.RawURLEncoding.EncodeToString(payloadJSON)
	signingInput := header + "." + payload

	var digest []byte
	var keySize int
	switch alg {
	case "ES256":
		h := sha256.Sum256([]byte(signingInput))
		digest, keySize = h[:], 32
	case "ES384":
		h := sha512.Sum384([]byte(signingInput))
		digest, keySize = h[:], 48
	default:
		t.Fatalf("unsupported alg %s", alg)
	}

	r, s, err := ecdsa.Sign(rand.Reader, priv, digest)
	if err != nil {
		t.Fatal(err)
	}
	sig := append(leftPad(r.Bytes(), keySize), leftPad(s.Bytes(), keySize)...)
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// --- JWKS Fetcher tests ---

func TestJWKSFetcher_FetchAndCache(t *testing.T) {
	key := generateRSAKey(t)
	srv := serveJWKS(t, rsaJWKJSON("key-1", &key.PublicKey))

	fetcher := NewJWKSFetcher(srv.URL, time.Minute, false)
	jwk, err := fetcher.GetKey("key-1")
	if err != nil {
		t.Fatalf("GetKey: %v", err)
	}
	if jwk.Kid != "key-1" {
		t.Errorf("kid = %q, want key-1", jwk.Kid)
	}

	// Parse the key and verify it's valid.
	pubKey, err := jwk.RSAPublicKey()
	if err != nil {
		t.Fatalf("RSAPublicKey: %v", err)
	}
	if pubKey.N.Cmp(key.PublicKey.N) != 0 {
		t.Error("parsed RSA key does not match original")
	}
}

func TestJWKSFetcher_UnknownKid(t *testing.T) {
	key := generateRSAKey(t)
	srv := serveJWKS(t, rsaJWKJSON("key-1", &key.PublicKey))

	fetcher := NewJWKSFetcher(srv.URL, time.Minute, false)
	fetcher.refreshDelay = 0 // allow immediate refresh for testing

	_, err := fetcher.GetKey("nonexistent")
	if err == nil {
		t.Fatal("expected error for unknown kid")
	}
}

func TestJWKSFetcher_KeyRotation(t *testing.T) {
	key1 := generateRSAKey(t)
	key2 := generateRSAKey(t)

	// Start serving only key-1.
	keys := make([]map[string]any, 0, 2)
	keys = append(keys, rsaJWKJSON("key-1", &key1.PublicKey))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"keys": keys})
	}))
	t.Cleanup(srv.Close)

	fetcher := NewJWKSFetcher(srv.URL, time.Minute, false)
	fetcher.refreshDelay = 0 // allow immediate refresh

	// Fetch key-1.
	if _, err := fetcher.GetKey("key-1"); err != nil {
		t.Fatalf("GetKey key-1: %v", err)
	}

	// key-2 doesn't exist yet — should fail.
	if _, err := fetcher.GetKey("key-2"); err == nil {
		t.Fatal("expected error for key-2 before rotation")
	}

	// Simulate key rotation: add key-2.
	keys = append(keys, rsaJWKJSON("key-2", &key2.PublicKey))

	// Force-refresh should find key-2.
	jwk, err := fetcher.GetKey("key-2")
	if err != nil {
		t.Fatalf("GetKey key-2 after rotation: %v", err)
	}
	if jwk.Kid != "key-2" {
		t.Errorf("kid = %q, want key-2", jwk.Kid)
	}
}

// flakyJWKS serves a JWKS whose availability can be toggled mid-test: while
// "up" it returns the configured keys, while "down" it returns 500. It makes the
// stale-cache (availability-vs-revocation) tradeoff testable without races.
type flakyJWKS struct {
	mu   sync.Mutex
	up   bool
	keys []map[string]any
}

func (f *flakyJWKS) setUp(up bool) {
	f.mu.Lock()
	f.up = up
	f.mu.Unlock()
}

func (f *flakyJWKS) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		up := f.up
		keys := f.keys
		f.mu.Unlock()
		if !up {
			http.Error(w, "unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"keys": keys}) //nolint:errcheck // test helper
	}
}

// TestJWKSFetcher_StaleCacheServedWhenEndpointDown documents the deliberate
// availability-over-revocation tradeoff: once a key is cached, a later refetch
// that fails (endpoint returns 500) does NOT evict the key — the stale cached
// key keeps validating tokens through a JWKS outage. Asserting this pins the
// behavior so a regression toward fail-closed (or, worse, silently returning the
// wrong key) is caught.
func TestJWKSFetcher_StaleCacheServedWhenEndpointDown(t *testing.T) {
	key := generateRSAKey(t)
	flaky := &flakyJWKS{up: true, keys: []map[string]any{rsaJWKJSON("key-1", &key.PublicKey)}}
	srv := httptest.NewServer(flaky.handler())
	t.Cleanup(srv.Close)

	fetcher := NewJWKSFetcher(srv.URL, time.Minute, false)

	// Warm the cache with a successful fetch.
	warm, err := fetcher.GetKey("key-1")
	if err != nil {
		t.Fatalf("warm GetKey: %v", err)
	}
	if warm.Kid != "key-1" {
		t.Fatalf("warm kid = %q, want key-1", warm.Kid)
	}

	// Expire the cache so the next GetKey is forced to refetch, then take the
	// endpoint down so that refetch fails.
	fetcher.mu.Lock()
	fetcher.lastFetch = time.Time{}
	fetcher.mu.Unlock()
	flaky.setUp(false)

	// Sanity: the cache is genuinely expired and a direct fetch now errors, so
	// the GetKey below truly exercises the stale-fallback branch (not the fresh
	// cache path).
	if err := fetcher.fetch(); err == nil {
		t.Fatal("expected fetch to fail while endpoint is down")
	}
	fetcher.mu.Lock()
	fetcher.lastFetch = time.Time{}
	fetcher.mu.Unlock()

	// The stale cached key must still be returned despite the failed refetch.
	got, err := fetcher.GetKey("key-1")
	if err != nil {
		t.Fatalf("GetKey during outage = %v, want stale key served", err)
	}
	if got.Kid != "key-1" {
		t.Errorf("stale kid = %q, want key-1", got.Kid)
	}
	if got.N != warm.N {
		t.Error("stale key material differs from the originally cached key")
	}
}

// TestJWKSFetcher_NoCacheFetchFailureReturnsError is the complement: when the
// endpoint is down and the kid was never cached, there is no stale key to fall
// back to, so GetKey must return an error rather than authenticating with no key.
func TestJWKSFetcher_NoCacheFetchFailureReturnsError(t *testing.T) {
	key := generateRSAKey(t)
	flaky := &flakyJWKS{up: false, keys: []map[string]any{rsaJWKJSON("key-1", &key.PublicKey)}}
	srv := httptest.NewServer(flaky.handler())
	t.Cleanup(srv.Close)

	fetcher := NewJWKSFetcher(srv.URL, time.Minute, false)

	if _, err := fetcher.GetKey("key-1"); err == nil {
		t.Fatal("expected error when endpoint is down and nothing is cached")
	}
}

func TestDiscoverJWKSURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{
			"jwks_uri": "https://auth.example.com/jwks",
		})
	}))
	t.Cleanup(srv.Close)

	url, err := DiscoverJWKSURL(srv.URL, false)
	if err != nil {
		t.Fatalf("DiscoverJWKSURL: %v", err)
	}
	if url != "https://auth.example.com/jwks" {
		t.Errorf("jwks_uri = %q, want https://auth.example.com/jwks", url)
	}
}

func TestDiscoverJWKSURL_Missing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"issuer": "https://example.com"})
	}))
	t.Cleanup(srv.Close)

	_, err := DiscoverJWKSURL(srv.URL, false)
	if err == nil {
		t.Fatal("expected error when jwks_uri is missing")
	}
}

// --- JWKS JWT middleware tests ---

func TestJWKSJWT_RS256Valid(t *testing.T) {
	key := generateRSAKey(t)
	srv := serveJWKS(t, rsaJWKJSON("key-1", &key.PublicKey))

	token := makeRSAJWT(t, map[string]any{
		"sub":   "service-a",
		"scope": "ingest",
		"exp":   float64(time.Now().Add(time.Hour).Unix()),
	}, "key-1", key)

	mw := JWKSJWT(JWKSJWTConfig{JWKSURL: srv.URL})

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
	if !ctx.User.HasScope("ingest") {
		t.Errorf("scopes = %v, want [ingest]", ctx.User.Scopes)
	}
}

func TestJWKSJWT_RS256Expired(t *testing.T) {
	key := generateRSAKey(t)
	srv := serveJWKS(t, rsaJWKJSON("key-1", &key.PublicKey))

	token := makeRSAJWT(t, map[string]any{
		"sub": "service-a",
		"exp": float64(time.Now().Add(-time.Hour).Unix()),
	}, "key-1", key)

	mw := JWKSJWT(JWKSJWTConfig{JWKSURL: srv.URL})

	req := httptest.NewRequest("POST", "/v1/traces", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	ctx := phttp.NewContext(httptest.NewRecorder(), req)

	mw(ctx, func() *phttp.Response { return phttp.JSON("ok") })

	if ctx.User != nil {
		t.Error("expected user to be nil for expired RS256 token")
	}
}

// --- JWKS/OIDC path requires exp by default ---

// TestJWKSJWT_RS256NoExpRejectedByDefault proves a validly-signed RS256 token
// with no "exp" claim is rejected by default, closing the never-expiring replay
// gap and matching the HS256 path.
func TestJWKSJWT_RS256NoExpRejectedByDefault(t *testing.T) {
	spectest.Proves(t, "go/authentication-authorization", "credential-validation", "jwks-rejects-missing-expiry-by-default")
	key := generateRSAKey(t)
	srv := serveJWKS(t, rsaJWKJSON("key-1", &key.PublicKey))

	token := makeRSAJWT(t, map[string]any{
		"sub": "service-a",
	}, "key-1", key)

	mw := JWKSJWT(JWKSJWTConfig{JWKSURL: srv.URL})

	req := httptest.NewRequest("POST", "/v1/traces", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	ctx := phttp.NewContext(httptest.NewRecorder(), req)

	mw(ctx, func() *phttp.Response { return phttp.JSON("ok") })

	if ctx.User != nil {
		t.Error("expected user to be nil — RS256 token without exp must be rejected by default")
	}
}

// TestJWKSJWT_ES256NoExpRejectedByDefault proves the same default-secure
// behavior on the ECDSA path.
func TestJWKSJWT_ES256NoExpRejectedByDefault(t *testing.T) {
	key := generateECKey(t, elliptic.P256())
	srv := serveJWKS(t, ecJWKJSON("ec-1", "ES256", &key.PublicKey))

	token := makeECJWT(t, map[string]any{
		"sub": "service-a",
	}, "ec-1", key, "ES256")

	mw := JWKSJWT(JWKSJWTConfig{JWKSURL: srv.URL})

	req := httptest.NewRequest("POST", "/v1/traces", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	ctx := phttp.NewContext(httptest.NewRecorder(), req)

	mw(ctx, func() *phttp.Response { return phttp.JSON("ok") })

	if ctx.User != nil {
		t.Error("expected user to be nil — ES256 token without exp must be rejected by default")
	}
}

// TestJWKSJWT_RS256NoExpAllowedWhenOptedIn proves a no-exp token is accepted
// only when AllowMissingExpiration is explicitly set.
func TestJWKSJWT_RS256NoExpAllowedWhenOptedIn(t *testing.T) {
	key := generateRSAKey(t)
	srv := serveJWKS(t, rsaJWKJSON("key-1", &key.PublicKey))

	token := makeRSAJWT(t, map[string]any{
		"sub": "service-a",
	}, "key-1", key)

	mw := JWKSJWT(JWKSJWTConfig{JWKSURL: srv.URL, AllowMissingExpiration: true})

	req := httptest.NewRequest("POST", "/v1/traces", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	ctx := phttp.NewContext(httptest.NewRecorder(), req)

	mw(ctx, func() *phttp.Response { return phttp.JSON("ok") })

	if ctx.User == nil {
		t.Fatal("expected user to be set — no-exp token must be accepted with AllowMissingExpiration")
	}
	if ctx.User.Subject != "service-a" {
		t.Errorf("subject = %q, want service-a", ctx.User.Subject)
	}
}

// TestJWKSJWT_ES256NoExpAllowedWhenOptedIn mirrors the opt-in case on ECDSA.
func TestJWKSJWT_ES256NoExpAllowedWhenOptedIn(t *testing.T) {
	key := generateECKey(t, elliptic.P256())
	srv := serveJWKS(t, ecJWKJSON("ec-1", "ES256", &key.PublicKey))

	token := makeECJWT(t, map[string]any{
		"sub": "service-a",
	}, "ec-1", key, "ES256")

	mw := JWKSJWT(JWKSJWTConfig{JWKSURL: srv.URL, AllowMissingExpiration: true})

	req := httptest.NewRequest("POST", "/v1/traces", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	ctx := phttp.NewContext(httptest.NewRecorder(), req)

	mw(ctx, func() *phttp.Response { return phttp.JSON("ok") })

	if ctx.User == nil {
		t.Fatal("expected user to be set — no-exp ES256 token must be accepted with AllowMissingExpiration")
	}
}

// makeHS256JWTWithKID forges an HS256 token carrying kid, signed with secret.
// It exists so the algorithm-confusion test can build the attack shape a naive
// resolver would accept: a published key's kid plus HMAC over material the
// attacker can read from the JWKS.
func makeHS256JWTWithKID(t *testing.T, claims map[string]any, kid, secret string) string {
	t.Helper()
	headerJSON, err := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT", "kid": kid})
	if err != nil {
		t.Fatal(err)
	}
	header := base64.RawURLEncoding.EncodeToString(headerJSON)
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	signingInput := header + "." + base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signingInput))
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func TestJWKSJWT_AlgorithmConfusion(t *testing.T) {
	spectest.Proves(t, "go/authentication-authorization", "credential-validation", "jwks-rejects-algorithm-confusion")
	// The real algorithm-confusion attack, not a merely malformed token: the
	// attacker takes the RSA PUBLIC key the JWKS publishes, uses its modulus
	// bytes as an HMAC secret, and signs HS256 with the kid of that same
	// published key. A resolver that looks the kid up and then trusts the
	// header's alg would verify the HMAC with material the attacker also has,
	// and the forgery would authenticate.
	//
	// Signing with a bogus secret and no kid would be rejected by key lookup
	// alone, so it would pass this test even with the alg guard removed — it
	// would prove nothing about algorithm confusion.
	key := generateRSAKey(t)
	srv := serveJWKS(t, rsaJWKJSON("key-1", &key.PublicKey))

	hmacToken := makeHS256JWTWithKID(t, map[string]any{
		"sub": "attacker",
		"exp": float64(time.Now().Add(time.Hour).Unix()),
	}, "key-1", string(key.PublicKey.N.Bytes()))

	mw := JWKSJWT(JWKSJWTConfig{JWKSURL: srv.URL})

	req := httptest.NewRequest("POST", "/", nil)
	req.Header.Set("Authorization", "Bearer "+hmacToken)
	ctx := phttp.NewContext(httptest.NewRecorder(), req)

	mw(ctx, func() *phttp.Response { return phttp.JSON("ok") })

	if ctx.User != nil {
		t.Error("expected user to be nil — HS256 must be rejected by JWKS resolver")
	}
}

func TestJWKSJWT_AudienceValid(t *testing.T) {
	key := generateRSAKey(t)
	srv := serveJWKS(t, rsaJWKJSON("key-1", &key.PublicKey))

	token := makeRSAJWT(t, map[string]any{
		"sub": "service-a",
		"aud": "otel-collector",
		"exp": float64(time.Now().Add(time.Hour).Unix()),
	}, "key-1", key)

	mw := JWKSJWT(JWKSJWTConfig{JWKSURL: srv.URL, Audience: "otel-collector"})

	req := httptest.NewRequest("POST", "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	ctx := phttp.NewContext(httptest.NewRecorder(), req)

	mw(ctx, func() *phttp.Response { return phttp.JSON("ok") })

	if ctx.User == nil {
		t.Fatal("expected user to be set for valid audience")
	}
}

func TestJWKSJWT_AudienceMismatch(t *testing.T) {
	spectest.Proves(t, "go/authentication-authorization", "credential-validation", "jwks-rejects-audience-mismatch")
	key := generateRSAKey(t)
	srv := serveJWKS(t, rsaJWKJSON("key-1", &key.PublicKey))

	token := makeRSAJWT(t, map[string]any{
		"sub": "service-a",
		"aud": "wrong-audience",
		"exp": float64(time.Now().Add(time.Hour).Unix()),
	}, "key-1", key)

	mw := JWKSJWT(JWKSJWTConfig{JWKSURL: srv.URL, Audience: "otel-collector"})

	req := httptest.NewRequest("POST", "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	ctx := phttp.NewContext(httptest.NewRecorder(), req)

	mw(ctx, func() *phttp.Response { return phttp.JSON("ok") })

	if ctx.User != nil {
		t.Error("expected user to be nil for mismatched audience")
	}
}

func TestJWKSJWT_AudienceArray(t *testing.T) {
	key := generateRSAKey(t)
	srv := serveJWKS(t, rsaJWKJSON("key-1", &key.PublicKey))

	token := makeRSAJWT(t, map[string]any{
		"sub": "service-a",
		"aud": []string{"other-service", "otel-collector"},
		"exp": float64(time.Now().Add(time.Hour).Unix()),
	}, "key-1", key)

	mw := JWKSJWT(JWKSJWTConfig{JWKSURL: srv.URL, Audience: "otel-collector"})

	req := httptest.NewRequest("POST", "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	ctx := phttp.NewContext(httptest.NewRecorder(), req)

	mw(ctx, func() *phttp.Response { return phttp.JSON("ok") })

	if ctx.User == nil {
		t.Fatal("expected user to be set for audience in array")
	}
}

func TestJWKSJWT_WithIssuerDiscovery(t *testing.T) {
	key := generateRSAKey(t)

	// Serve both OIDC discovery and JWKS from the same test server.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			// Point jwks_uri to ourselves.
			json.NewEncoder(w).Encode(map[string]string{
				"jwks_uri": fmt.Sprintf("http://%s/jwks", r.Host),
			})
		case "/jwks":
			json.NewEncoder(w).Encode(map[string]any{
				"keys": []any{rsaJWKJSON("key-1", &key.PublicKey)},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	token := makeRSAJWT(t, map[string]any{
		"sub": "service-a",
		"iss": srv.URL, // OIDC discovery issuer is now enforced as the required iss
		"exp": float64(time.Now().Add(time.Hour).Unix()),
	}, "key-1", key)

	mw := JWKSJWT(JWKSJWTConfig{Issuer: srv.URL})

	req := httptest.NewRequest("POST", "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	ctx := phttp.NewContext(httptest.NewRecorder(), req)

	mw(ctx, func() *phttp.Response { return phttp.JSON("ok") })

	if ctx.User == nil {
		t.Fatal("expected user to be set via OIDC discovery")
	}
	if ctx.User.Subject != "service-a" {
		t.Errorf("subject = %q, want service-a", ctx.User.Subject)
	}
}

// --- JWKS/OIDC path enforces the iss claim ---

// TestJWKSJWT_IssuerDiscoveryMismatchRejected proves the OIDC discovery issuer
// now doubles as the required "iss": a validly-signed token from the same JWKS
// but carrying a foreign iss must be rejected (token-confusion guard).
func TestJWKSJWT_IssuerDiscoveryMismatchRejected(t *testing.T) {
	spectest.Proves(t, "go/authentication-authorization", "credential-validation", "jwks-rejects-issuer-mismatch")
	key := generateRSAKey(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			json.NewEncoder(w).Encode(map[string]string{"jwks_uri": fmt.Sprintf("http://%s/jwks", r.Host)})
		case "/jwks":
			json.NewEncoder(w).Encode(map[string]any{"keys": []any{rsaJWKJSON("key-1", &key.PublicKey)}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	token := makeRSAJWT(t, map[string]any{
		"sub": "attacker",
		"iss": "https://evil.example.com", // signed by the same JWKS, wrong issuer
		"exp": float64(time.Now().Add(time.Hour).Unix()),
	}, "key-1", key)

	mw := JWKSJWT(JWKSJWTConfig{Issuer: srv.URL})

	req := httptest.NewRequest("POST", "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	ctx := phttp.NewContext(httptest.NewRecorder(), req)
	mw(ctx, func() *phttp.Response { return phttp.JSON("ok") })

	if ctx.User != nil {
		t.Error("expected user to be nil — token with a foreign iss must be rejected")
	}
}

// TestJWKSJWT_RequiredIssuerWithJWKSURL covers the JWKSURL path, where there is
// no discovery issuer: RequiredIssuer must enforce iss (accept on match, reject
// on mismatch).
func TestJWKSJWT_RequiredIssuerWithJWKSURL(t *testing.T) {
	key := generateRSAKey(t)
	srv := serveJWKS(t, rsaJWKJSON("key-1", &key.PublicKey))

	run := func(iss string) *phttp.Context {
		token := makeRSAJWT(t, map[string]any{
			"sub": "service-a",
			"iss": iss,
			"exp": float64(time.Now().Add(time.Hour).Unix()),
		}, "key-1", key)
		mw := JWKSJWT(JWKSJWTConfig{JWKSURL: srv.URL, RequiredIssuer: "https://issuer.example.com"})
		req := httptest.NewRequest("POST", "/", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		ctx := phttp.NewContext(httptest.NewRecorder(), req)
		mw(ctx, func() *phttp.Response { return phttp.JSON("ok") })
		return ctx
	}

	if ctx := run("https://issuer.example.com"); ctx.User == nil {
		t.Error("matching iss should be accepted with RequiredIssuer")
	}
	if ctx := run("https://issuer.other.com"); ctx.User != nil {
		t.Error("mismatched iss should be rejected with RequiredIssuer")
	}
}

// --- The iss claim gates the JWKS fetch for chained foreign-issuer tokens ---

// TestJWKSJWT_ForeignIssuerSkipsJWKSFetch proves the "iss" claim is checked
// before any network I/O. A token whose issuer does not match the resolver's
// configured issuer is rejected WITHOUT fetching — or force-refreshing — the
// resolver's JWKS. In a chained-resolver setup this keeps a foreign token (meant
// for a later resolver) from triggering a needless hot-path JWKS refresh against
// an earlier resolver's endpoint; the matching-issuer case still fetches
// and authenticates, so the fix does not regress the normal path.
func TestJWKSJWT_ForeignIssuerSkipsJWKSFetch(t *testing.T) {
	key := generateRSAKey(t)

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"keys": []any{rsaJWKJSON("key-1", &key.PublicKey)}}) //nolint:errcheck // test helper
	}))
	t.Cleanup(srv.Close)

	const ourIssuer = "https://issuer-a.example.com"

	// Each run uses a fresh resolver so the fetcher cache never carries across
	// cases; hits is reset so it counts only the request under test.
	run := func(iss string) *phttp.Context {
		atomic.StoreInt32(&hits, 0)
		token := makeRSAJWT(t, map[string]any{
			"sub": "service",
			"iss": iss,
			"exp": float64(time.Now().Add(time.Hour).Unix()),
		}, "key-1", key)
		mw := JWKSJWT(JWKSJWTConfig{JWKSURL: srv.URL, RequiredIssuer: ourIssuer})
		req := httptest.NewRequest("POST", "/", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		ctx := phttp.NewContext(httptest.NewRecorder(), req)
		mw(ctx, func() *phttp.Response { return phttp.JSON("ok") })
		return ctx
	}

	// Foreign issuer: rejected, and the JWKS endpoint is never contacted.
	if ctx := run("https://issuer-b.example.com"); ctx.User != nil {
		t.Error("foreign-issuer token must be rejected")
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Errorf("JWKS endpoint hit %d time(s) for a foreign-issuer token; want 0 (iss must short-circuit before the fetch)", got)
	}

	// Matching issuer: authenticated, which requires the JWKS to be fetched.
	if ctx := run(ourIssuer); ctx.User == nil {
		t.Fatal("matching-issuer token must be accepted")
	}
	if got := atomic.LoadInt32(&hits); got == 0 {
		t.Error("JWKS endpoint was not contacted for a matching-issuer token; the fetch must still happen")
	}
}

func TestJWKSFetcher_Concurrency(t *testing.T) {
	key1 := generateRSAKey(t)
	key2 := generateRSAKey(t)

	// Mutable key set — starts with key-1, key-2 added mid-test to simulate rotation.
	var mu sync.Mutex
	keys := make([]map[string]any, 0, 2)
	keys = append(keys, rsaJWKJSON("key-1", &key1.PublicKey))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		snapshot := make([]map[string]any, len(keys))
		copy(snapshot, keys)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"keys": snapshot})
	}))
	t.Cleanup(srv.Close)

	fetcher := NewJWKSFetcher(srv.URL, 50*time.Millisecond, false) // short TTL to force re-fetches
	fetcher.refreshDelay = 0                                       // allow immediate refresh

	const numReaders = 20
	const readsPerGoroutine = 50

	// Phase 1: concurrent reads for key-1.
	var wg sync.WaitGroup
	errs := make(chan error, numReaders*readsPerGoroutine)
	for i := 0; i < numReaders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < readsPerGoroutine; j++ {
				jwk, err := fetcher.GetKey("key-1")
				if err != nil {
					errs <- fmt.Errorf("GetKey(key-1): %w", err)
					return
				}
				if jwk.Kid != "key-1" {
					errs <- fmt.Errorf("kid = %q, want key-1", jwk.Kid)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	// Phase 2: concurrent reads while a rotation happens.
	mu.Lock()
	keys = append(keys, rsaJWKJSON("key-2", &key2.PublicKey))
	mu.Unlock()

	// Expire the cache so the next GetKey triggers a fetch.
	fetcher.mu.Lock()
	fetcher.lastFetch = time.Time{}
	fetcher.mu.Unlock()

	errs2 := make(chan error, numReaders*readsPerGoroutine)
	var wg2 sync.WaitGroup
	for i := 0; i < numReaders; i++ {
		wg2.Add(1)
		kid := "key-1"
		if i%2 == 0 {
			kid = "key-2"
		}
		go func(kid string) {
			defer wg2.Done()
			for j := 0; j < readsPerGoroutine; j++ {
				jwk, err := fetcher.GetKey(kid)
				if err != nil {
					errs2 <- fmt.Errorf("GetKey(%s): %w", kid, err)
					return
				}
				if jwk.Kid != kid {
					errs2 <- fmt.Errorf("kid = %q, want %s", jwk.Kid, kid)
					return
				}
			}
		}(kid)
	}
	wg2.Wait()
	close(errs2)
	for err := range errs2 {
		t.Error(err)
	}
}

func TestJWKSJWT_NoAuthHeader(t *testing.T) {
	key := generateRSAKey(t)
	srv := serveJWKS(t, rsaJWKJSON("key-1", &key.PublicKey))

	mw := JWKSJWT(JWKSJWTConfig{JWKSURL: srv.URL})

	req := httptest.NewRequest("POST", "/", nil)
	ctx := phttp.NewContext(httptest.NewRecorder(), req)

	mw(ctx, func() *phttp.Response { return phttp.JSON("ok") })

	if ctx.User != nil {
		t.Error("expected user to be nil when no auth header")
	}
}

// --- ECDSA (ES256 / ES384) verification tests ---

func TestJWKSJWT_ES256Valid(t *testing.T) {
	key := generateECKey(t, elliptic.P256())
	srv := serveJWKS(t, ecJWKJSON("ec-1", "ES256", &key.PublicKey))

	token := makeECJWT(t, map[string]any{
		"sub":   "service-a",
		"scope": "ingest",
		"exp":   float64(time.Now().Add(time.Hour).Unix()),
	}, "ec-1", key, "ES256")

	mw := JWKSJWT(JWKSJWTConfig{JWKSURL: srv.URL})

	req := httptest.NewRequest("POST", "/v1/traces", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	ctx := phttp.NewContext(httptest.NewRecorder(), req)

	mw(ctx, func() *phttp.Response { return phttp.JSON("ok") })

	if ctx.User == nil {
		t.Fatal("expected user to be set for valid ES256 token")
	}
	if ctx.User.Subject != "service-a" {
		t.Errorf("subject = %q, want service-a", ctx.User.Subject)
	}
	if !ctx.User.HasScope("ingest") {
		t.Errorf("scopes = %v, want [ingest]", ctx.User.Scopes)
	}
}

func TestJWKSJWT_ES384Valid(t *testing.T) {
	key := generateECKey(t, elliptic.P384())
	srv := serveJWKS(t, ecJWKJSON("ec-2", "ES384", &key.PublicKey))

	token := makeECJWT(t, map[string]any{
		"sub": "service-a",
		"exp": float64(time.Now().Add(time.Hour).Unix()),
	}, "ec-2", key, "ES384")

	mw := JWKSJWT(JWKSJWTConfig{JWKSURL: srv.URL})

	req := httptest.NewRequest("POST", "/v1/traces", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	ctx := phttp.NewContext(httptest.NewRecorder(), req)

	mw(ctx, func() *phttp.Response { return phttp.JSON("ok") })

	if ctx.User == nil {
		t.Fatal("expected user to be set for valid ES384 token")
	}
	if ctx.User.Subject != "service-a" {
		t.Errorf("subject = %q, want service-a", ctx.User.Subject)
	}
}

func TestJWKSJWT_ES256Expired(t *testing.T) {
	key := generateECKey(t, elliptic.P256())
	srv := serveJWKS(t, ecJWKJSON("ec-1", "ES256", &key.PublicKey))

	token := makeECJWT(t, map[string]any{
		"sub": "service-a",
		"exp": float64(time.Now().Add(-time.Hour).Unix()),
	}, "ec-1", key, "ES256")

	mw := JWKSJWT(JWKSJWTConfig{JWKSURL: srv.URL})

	req := httptest.NewRequest("POST", "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	ctx := phttp.NewContext(httptest.NewRecorder(), req)

	mw(ctx, func() *phttp.Response { return phttp.JSON("ok") })

	if ctx.User != nil {
		t.Error("expected user to be nil for expired ES256 token")
	}
}

func TestJWKSJWT_ES256ForgedSignature(t *testing.T) {
	spectest.Proves(t, "go/authentication-authorization", "credential-validation", "jwks-rejects-forged-signature")
	// Token signed by an attacker key whose public key is NOT in the JWKS.
	served := generateECKey(t, elliptic.P256())
	attacker := generateECKey(t, elliptic.P256())
	srv := serveJWKS(t, ecJWKJSON("ec-1", "ES256", &served.PublicKey))

	token := makeECJWT(t, map[string]any{
		"sub": "attacker",
		"exp": float64(time.Now().Add(time.Hour).Unix()),
	}, "ec-1", attacker, "ES256")

	mw := JWKSJWT(JWKSJWTConfig{JWKSURL: srv.URL})

	req := httptest.NewRequest("POST", "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	ctx := phttp.NewContext(httptest.NewRecorder(), req)

	mw(ctx, func() *phttp.Response { return phttp.JSON("ok") })

	if ctx.User != nil {
		t.Error("expected user to be nil — ES256 signature from a non-published key must be rejected")
	}
}

func TestVerifyECDSA_WrongSignatureLength(t *testing.T) {
	key := generateECKey(t, elliptic.P256())
	// A P-256 signature must be exactly 64 bytes (32-byte R || 32-byte S).
	if err := verifyECDSA("header.payload", []byte{0x01, 0x02, 0x03}, &key.PublicKey, crypto.SHA256, 32); err == nil {
		t.Fatal("expected error for wrong-length ECDSA signature")
	}
}

func TestECDSAPublicKey_UnsupportedCurve(t *testing.T) {
	jwk := &JWK{Kty: "EC", Crv: "P-521", X: "AA", Y: "AA"}
	if _, err := jwk.ECDSAPublicKey(); err == nil {
		t.Fatal("expected error for unsupported curve P-521")
	}
}

// --- HTTPS transport hardening tests ---

func TestRequireSecureURL(t *testing.T) {
	cases := []struct {
		name          string
		url           string
		allowInsecure bool
		wantErr       bool
	}{
		{"https accepted", "https://issuer.example.com/jwks", false, false},
		{"http non-loopback rejected", "http://issuer.example.com/jwks", false, true},
		{"http non-loopback allowed when insecure", "http://issuer.example.com/jwks", true, false},
		{"http loopback ipv4 accepted", "http://127.0.0.1:8080/jwks", false, false},
		{"http localhost accepted", "http://localhost:8080/jwks", false, false},
		{"http loopback ipv6 accepted", "http://[::1]:8080/jwks", false, false},
		{"non-http scheme rejected", "ftp://issuer.example.com/jwks", false, true},
		{"non-http scheme rejected even when insecure", "ftp://issuer.example.com/jwks", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := requireSecureURL(tc.url, tc.allowInsecure)
			if tc.wantErr && err == nil {
				t.Errorf("requireSecureURL(%q, %v) = nil, want error", tc.url, tc.allowInsecure)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("requireSecureURL(%q, %v) = %v, want nil", tc.url, tc.allowInsecure, err)
			}
		})
	}
}

func TestJWKSFetcher_RejectsInsecureURL(t *testing.T) {
	// Non-loopback http must be rejected before any network call is made.
	fetcher := NewJWKSFetcher("http://keys.example.com/jwks", time.Minute, false)
	if _, err := fetcher.GetKey("any"); err == nil {
		t.Fatal("expected error fetching signing keys over insecure http")
	}
}

func TestDiscoverJWKSURL_RejectsInsecureIssuer(t *testing.T) {
	if _, err := DiscoverJWKSURL("http://issuer.example.com", false); err == nil {
		t.Fatal("expected error discovering JWKS over insecure http issuer")
	}
}

func TestDiscoverJWKSURL_RejectsInsecureJWKSURI(t *testing.T) {
	// Discovery served over loopback http (allowed), but it points jwks_uri
	// at a non-loopback plaintext endpoint, which must be rejected.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"jwks_uri": "http://keys.example.com/jwks"}) //nolint:errcheck // test helper
	}))
	t.Cleanup(srv.Close)

	if _, err := DiscoverJWKSURL(srv.URL, false); err == nil {
		t.Fatal("expected error for insecure discovered jwks_uri")
	}
}
