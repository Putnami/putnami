package security

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	phttp "go.putnami.dev/http"
)

// countingJWKS serves the given key JSON maps and counts how many times the
// endpoint was hit, so a test can assert seeded kids never reach the network
// and an unknown kid refreshes exactly once.
func countingJWKS(t *testing.T, hits *atomic.Int64, keys ...map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// jwkFromMap converts the JSON-map JWK form used by the other test helpers into
// the typed JWK struct that seeds a fetcher.
func jwkFromMap(t *testing.T, m map[string]any) JWK {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var k JWK
	if err := json.Unmarshal(raw, &k); err != nil {
		t.Fatal(err)
	}
	return k
}

func TestSeededJWKSFetcher_SeededKidVerifiesOffline(t *testing.T) {
	priv := generateRSAKey(t)
	seed := jwkFromMap(t, rsaJWKJSON("kid-a", &priv.PublicKey))

	var hits atomic.Int64
	srv := countingJWKS(t, &hits) // serves nothing; asserts it is never hit

	f := NewSeededJWKSFetcher(srv.URL, []JWK{seed}, time.Hour, true)
	got, err := f.GetKey("kid-a")
	if err != nil {
		t.Fatalf("GetKey(seeded) error: %v", err)
	}
	if got == nil || got.Kid != "kid-a" {
		t.Fatalf("GetKey(seeded) = %+v, want kid-a", got)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("seeded kid hit the JWKS endpoint %d times, want 0", n)
	}
}

func TestSeededJWKSFetcher_UnknownKidRefreshesOnce(t *testing.T) {
	seedPriv := generateRSAKey(t)
	seed := jwkFromMap(t, rsaJWKJSON("kid-a", &seedPriv.PublicKey))

	rotPriv := generateRSAKey(t)
	var hits atomic.Int64
	srv := countingJWKS(t, &hits, rsaJWKJSON("kid-b", &rotPriv.PublicKey))

	f := NewSeededJWKSFetcher(srv.URL, []JWK{seed}, time.Hour, true)

	// Unknown kid (rotation) triggers a network refresh and resolves.
	got, err := f.GetKey("kid-b")
	if err != nil {
		t.Fatalf("GetKey(rotated) error: %v", err)
	}
	if got == nil || got.Kid != "kid-b" {
		t.Fatalf("GetKey(rotated) = %+v, want kid-b", got)
	}
	// A repeat within cacheTTL is served from the fetched cache: still one hit.
	if _, err := f.GetKey("kid-b"); err != nil {
		t.Fatalf("second GetKey(rotated) error: %v", err)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("unknown kid caused %d refreshes, want exactly 1", n)
	}
	// The seeded kid is still served offline after the refresh.
	if _, err := f.GetKey("kid-a"); err != nil {
		t.Fatalf("GetKey(seeded after refresh) error: %v", err)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("seeded kid caused an extra refresh: %d hits, want 1", n)
	}
}

func TestSeededJWKSFetcher_StaleWhileRevalidateOnOutage(t *testing.T) {
	priv := generateRSAKey(t)
	seed := jwkFromMap(t, rsaJWKJSON("kid-a", &priv.PublicKey))

	// Endpoint that always fails — simulates a JWKS/auth-server outage.
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(down.Close)

	f := NewSeededJWKSFetcher(down.URL, []JWK{seed}, time.Hour, true)

	// Seeded kid keeps verifying through the outage.
	if _, err := f.GetKey("kid-a"); err != nil {
		t.Fatalf("seeded kid failed during outage: %v", err)
	}
	// An unknown kid can't be resolved while the endpoint is down.
	if _, err := f.GetKey("kid-unknown"); err == nil {
		t.Error("unknown kid resolved during outage, want error")
	}
}

func TestSeededJWKSFetcher_OfflineNoURL(t *testing.T) {
	priv := generateRSAKey(t)
	seed := jwkFromMap(t, rsaJWKJSON("kid-a", &priv.PublicKey))

	f := NewSeededJWKSFetcher("", []JWK{seed}, time.Hour, true)

	if _, err := f.GetKey("kid-a"); err != nil {
		t.Fatalf("seeded kid failed with no URL: %v", err)
	}
	// No endpoint to refresh from: unknown kid fails cleanly (no doomed fetch).
	if _, err := f.GetKey("kid-unknown"); err == nil {
		t.Error("unknown kid resolved with no URL, want error")
	}
}

func TestJWKSJWT_SeedKeys_VerifiesOfflineAndRejectsUnknownKid(t *testing.T) {
	priv := generateRSAKey(t)
	seed := jwkFromMap(t, rsaJWKJSON("kid-a", &priv.PublicKey))

	// No JWKSURL and no reachable issuer: verification must work purely from the
	// seed, proving auth-server is off the JWT-verification boot path.
	mw := JWKSJWT(JWKSJWTConfig{SeedKeys: []JWK{seed}})

	valid := makeRSAJWT(t, map[string]any{
		"sub": "svc-a",
		"exp": float64(time.Now().Add(time.Hour).Unix()),
	}, "kid-a", priv)

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer "+valid)
	ctx := phttp.NewContext(httptest.NewRecorder(), req)
	mw(ctx, func() *phttp.Response { return phttp.JSON("ok") })
	if ctx.User == nil || ctx.User.Subject != "svc-a" {
		t.Fatalf("seeded JWT did not verify offline: user=%+v", ctx.User)
	}

	// A token signed by a key whose kid is not seeded, with no endpoint to
	// refresh from, must be rejected.
	otherPriv := generateRSAKey(t)
	unknown := makeRSAJWT(t, map[string]any{
		"sub": "svc-z",
		"exp": float64(time.Now().Add(time.Hour).Unix()),
	}, "kid-z", otherPriv)
	req2 := httptest.NewRequest("GET", "/", nil)
	req2.Header.Set("Authorization", "Bearer "+unknown)
	ctx2 := phttp.NewContext(httptest.NewRecorder(), req2)
	mw(ctx2, func() *phttp.Response { return phttp.JSON("ok") })
	if ctx2.User != nil {
		t.Errorf("unknown-kid token verified, want rejection: user=%+v", ctx2.User)
	}
}

func TestJWKSJWT_SeedKeys_ToleratesDiscoveryOutage(t *testing.T) {
	priv := generateRSAKey(t)
	seed := jwkFromMap(t, rsaJWKJSON("kid-a", &priv.PublicKey))

	// Issuer points at a closed server so OIDC discovery fails. A token with an
	// unseeded kid triggers that discovery first; with a seed the resolver must
	// still verify seeded tokens rather than disabling itself on the error.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	mw := JWKSJWT(JWKSJWTConfig{Issuer: deadURL, SeedKeys: []JWK{seed}, AllowInsecure: true})

	unseeded := makeRSAJWT(t, map[string]any{
		"sub": "svc-a",
		"iss": deadURL,
		"exp": float64(time.Now().Add(time.Hour).Unix()),
	}, "kid-z", generateRSAKey(t))
	if user := bearerUser(mw, unseeded); user != nil {
		t.Fatalf("unseeded kid verified while discovery fails: user=%+v", user)
	}

	// Issuer is set, so the "iss" claim is enforced; it must match deadURL even
	// though discovery against it fails.
	valid := makeRSAJWT(t, map[string]any{
		"sub": "svc-a",
		"iss": deadURL,
		"exp": float64(time.Now().Add(time.Hour).Unix()),
	}, "kid-a", priv)
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer "+valid)
	ctx := phttp.NewContext(httptest.NewRecorder(), req)
	mw(ctx, func() *phttp.Response { return phttp.JSON("ok") })
	if ctx.User == nil || ctx.User.Subject != "svc-a" {
		t.Fatalf("seeded JWT did not verify through discovery outage: user=%+v", ctx.User)
	}
}

func TestParseJWKS(t *testing.T) {
	priv := generateRSAKey(t)
	doc, err := json.Marshal(map[string]any{"keys": []map[string]any{rsaJWKJSON("k1", &priv.PublicKey)}})
	if err != nil {
		t.Fatal(err)
	}
	jwks, err := ParseJWKS(string(doc))
	if err != nil {
		t.Fatalf("ParseJWKS error: %v", err)
	}
	if len(jwks.Keys) != 1 || jwks.Keys[0].Kid != "k1" {
		t.Fatalf("keys = %+v, want one kid k1", jwks.Keys)
	}
	// Empty is a no-op, not an error.
	if got, err := ParseJWKS(""); err != nil || len(got.Keys) != 0 {
		t.Errorf("ParseJWKS(\"\") = (%+v, %v), want (empty, nil)", got, err)
	}
	// Malformed JSON surfaces an error.
	if _, err := ParseJWKS("{not json"); err == nil {
		t.Error("ParseJWKS(malformed) = nil error, want error")
	}
}
