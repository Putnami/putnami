package security

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	phttp "go.putnami.dev/http"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/protocol/keyring"
)

// Fixture paths shared with the keyring conformance corpus. Reusing them
// keeps the sign side byte-consistent with the cross-language vectors.
const (
	jwtVectorsPath = "../../../protocols/keyring/fixtures/jwt-vectors.json"
	keyringsPath   = "../../../protocols/keyring/fixtures/keyrings.json"
	keyStatesPath  = "../../../protocols/keyring/fixtures/key-states.json"
)

// --- Shared test helpers ---

// standardClaims returns a claim set that satisfies the default verify policy
// (a future exp is required) plus a fixed iss/aud so a verifier can enforce them.
func standardClaims() map[string]any {
	return map[string]any{
		"sub": "user-123",
		"iss": "https://issuer.example",
		"aud": "putnami",
		"exp": 4102444800, // year 2100
		"iat": 1700000000,
	}
}

// verifyThroughExistingPath proves a token verifies through the UNCHANGED verify
// path: it projects the provider's public JWKS to JSON, feeds it to the real
// consumer parser (security.ParseJWKS), seeds a JWKSFetcher, and runs
// validateJWKS/verifyECDSA/verifyRSA256. It returns the resolved claims.
func verifyThroughExistingPath(t *testing.T, jwks keyring.JWKS, token, audience, issuer string) *phttp.Claims {
	t.Helper()
	raw, err := json.Marshal(jwks)
	if err != nil {
		t.Fatalf("marshal jwks: %v", err)
	}
	parsed, err := ParseJWKS(string(raw))
	if err != nil {
		t.Fatalf("ParseJWKS: %v", err)
	}
	fetcher := NewSeededJWKSFetcher("", parsed.Keys, time.Hour, false)
	claims, err := validateJWKS(token, fetcher, audience, issuer, true)
	if err != nil {
		t.Fatalf("validateJWKS rejected a self-signed token: %v", err)
	}
	if claims == nil {
		t.Fatal("validateJWKS returned nil claims")
	}
	return claims
}

func loadKeyringsFixture(t *testing.T) *keyring.PrivateKeyring {
	t.Helper()
	data, err := os.ReadFile(keyringsPath)
	if err != nil {
		t.Fatalf("read keyrings fixture: %v", err)
	}
	var file struct {
		Cases []struct {
			ID      string                 `json:"id"`
			Private keyring.PrivateKeyring `json:"private"`
			Public  keyring.JWKS           `json:"public"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatalf("parse keyrings fixture: %v", err)
	}
	if len(file.Cases) == 0 {
		t.Fatal("keyrings fixture has no cases")
	}
	return &file.Cases[0].Private
}

func fixturePrivateJWK(t *testing.T, kr *keyring.PrivateKeyring, kid string) keyring.PrivateJWK {
	t.Helper()
	for _, k := range kr.Keys {
		if k.Kid == kid {
			return k
		}
	}
	t.Fatalf("fixture key %q not found", kid)
	return keyring.PrivateJWK{}
}

// ecFixtureSigningKey parses the ec-active fixture key (the same P-256 key that
// signed the ES256 JWT vector) into a live active SigningKey.
func ecFixtureSigningKey(t *testing.T) SigningKey {
	t.Helper()
	kr := loadKeyringsFixture(t)
	ec, err := parseECPrivateKey(fixturePrivateJWK(t, kr, "ec-active"))
	if err != nil {
		t.Fatalf("parse ec-active: %v", err)
	}
	return SigningKey{Kid: "ec-1", Alg: AlgES256, State: keyring.KeyStateActive, EC: ec}
}

// rsaFixtureSigningKey parses the rsa-retiring fixture key (full private
// material) into a live active RS256 SigningKey.
func rsaFixtureSigningKey(t *testing.T) SigningKey {
	t.Helper()
	kr := loadKeyringsFixture(t)
	rsaKey, err := parseRSAPrivateKey(fixturePrivateJWK(t, kr, "rsa-retiring"))
	if err != nil {
		t.Fatalf("parse rsa-retiring: %v", err)
	}
	return SigningKey{Kid: "rsa-1", Alg: AlgRS256, State: keyring.KeyStateActive, RSA: rsaKey}
}

func newECKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate EC key: %v", err)
	}
	return k
}

// snapshotActiveCount reads the concrete provider's current snapshot and counts
// active keys. It exists to pin the exactly-one-active invariant directly under
// concurrency; the atomic Load is race-safe and the snapshot is immutable.
func snapshotActiveCount(t *testing.T, p SigningKeyProvider) int {
	t.Helper()
	ip, ok := p.(*inMemoryProvider)
	if !ok {
		t.Fatal("provider is not *inMemoryProvider")
	}
	n := 0
	for _, k := range ip.state.Load().keys {
		if k.state == keyring.KeyStateActive {
			n++
		}
	}
	return n
}

// --- Round-trip: a signed token verifies through the existing verify path ---

func TestSign_ES256_RoundTripThroughVerifyPath(t *testing.T) {
	p, err := NewSigningKeyProvider(ProviderConfig{Keys: []SigningKey{
		ecFixtureSigningKey(t),
	}})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	if p.ActiveAlg() != AlgES256 {
		t.Fatalf("default active alg = %q, want ES256", p.ActiveAlg())
	}
	token, err := p.Sign(standardClaims())
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	claims := verifyThroughExistingPath(t, p.PublicJWKS(), token, "putnami", "https://issuer.example")
	if claims.Subject != "user-123" {
		t.Fatalf("subject = %q, want user-123", claims.Subject)
	}
}

func TestSign_RS256_RoundTripThroughVerifyPath(t *testing.T) {
	p, err := NewSigningKeyProvider(ProviderConfig{Keys: []SigningKey{
		rsaFixtureSigningKey(t),
	}})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	if p.ActiveAlg() != AlgRS256 {
		t.Fatalf("active alg = %q, want RS256", p.ActiveAlg())
	}
	token, err := p.Sign(standardClaims())
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	verifyThroughExistingPath(t, p.PublicJWKS(), token, "putnami", "https://issuer.example")
}

// TestSign_HeaderCanonicalization pins that the emitted JOSE header carries the
// required alg/kid/typ and that the signing input is base64url(header).payload,
// i.e. exactly what parseJWTHeader decodes.
func TestSign_HeaderCanonicalization(t *testing.T) {
	p, err := NewSigningKeyProvider(ProviderConfig{Keys: []SigningKey{
		ecFixtureSigningKey(t),
	}})
	if err != nil {
		t.Fatal(err)
	}
	token, err := p.Sign(standardClaims())
	if err != nil {
		t.Fatal(err)
	}
	header, parts, err := parseJWTHeader(token)
	if err != nil {
		t.Fatalf("parseJWTHeader on self-signed token: %v", err)
	}
	if len(parts) != 3 {
		t.Fatalf("token has %d segments, want 3", len(parts))
	}
	if header.Alg != "ES256" || header.Kid != "ec-1" || header.Typ != "JWT" {
		t.Fatalf("header = %+v, want alg=ES256 kid=ec-1 typ=JWT", header)
	}
}

// --- Exactly one active key ---

func TestProvider_RejectsZeroOrMultipleActive(t *testing.T) {
	// Two active keys are rejected.
	_, err := NewSigningKeyProvider(ProviderConfig{Keys: []SigningKey{
		{Kid: "a", EC: newECKey(t)},
		{Kid: "b", EC: newECKey(t)},
	}})
	if err == nil {
		t.Fatal("expected error for two active keys")
	}

	// Keys supplied but none active is rejected (not silently ephemeral-generated).
	_, err = NewSigningKeyProvider(ProviderConfig{Keys: []SigningKey{
		{Kid: "a", State: keyring.KeyStateRetiring, EC: newECKey(t)},
	}})
	if err == nil {
		t.Fatal("expected error when no key is active")
	}

	// Exactly one active is accepted.
	if _, err := NewSigningKeyProvider(ProviderConfig{Keys: []SigningKey{
		{Kid: "a", EC: newECKey(t)},
		{Kid: "b", State: keyring.KeyStateRetiring, EC: newECKey(t)},
	}}); err != nil {
		t.Fatalf("one active + one retiring rejected: %v", err)
	}
}

func TestProvider_RejectsDuplicateKid(t *testing.T) {
	_, err := NewSigningKeyProvider(ProviderConfig{Keys: []SigningKey{
		{Kid: "dup", EC: newECKey(t)},
		{Kid: "dup", State: keyring.KeyStateRetiring, EC: newECKey(t)},
	}})
	if err == nil {
		t.Fatal("expected duplicate-kid error")
	}
}

func TestRotate_PromotesNewActiveDemotesOld(t *testing.T) {
	p, err := NewSigningKeyProvider(ProviderConfig{Keys: []SigningKey{
		{Kid: "k1", EC: newECKey(t)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if p.ActiveKID() != "k1" {
		t.Fatalf("active kid = %q, want k1", p.ActiveKID())
	}

	ip := p.(*inMemoryProvider)
	if err := ip.Rotate(SigningKey{Kid: "k2", EC: newECKey(t)}); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if p.ActiveKID() != "k2" {
		t.Fatalf("after rotate active kid = %q, want k2", p.ActiveKID())
	}
	if got := snapshotActiveCount(t, p); got != 1 {
		t.Fatalf("active key count = %d, want 1", got)
	}

	// The previous active (k1) is now retiring and STILL published, so a token
	// signed just before rotation still verifies.
	jwks := p.PublicJWKS()
	kids := jwkKids(jwks)
	if !kids["k1"] || !kids["k2"] {
		t.Fatalf("published kids = %v, want both k1 (retiring) and k2 (active)", kids)
	}

	// Rotating in an existing kid is rejected.
	if err := ip.Rotate(SigningKey{Kid: "k2", EC: newECKey(t)}); err == nil {
		t.Fatal("expected error rotating in an existing kid")
	}
}

func jwkKids(jwks keyring.JWKS) map[string]bool {
	out := make(map[string]bool, len(jwks.Keys))
	for _, k := range jwks.Keys {
		out[k.Kid] = true
	}
	return out
}

// --- Retiring overlap window ---

func TestRetiringKey_LeavesJWKSPastOverlapWindow(t *testing.T) {
	spectest.Proves(t, "go/authentication-authorization", "jwks-overlap-and-revocation", "retiring-key-leaves-the-jwks-past-its-overlap-window")
	now := time.Now()
	clock := func() time.Time { return now }
	p, err := newProvider(ProviderConfig{
		OverlapWindow: time.Hour,
		Keys:          []SigningKey{{Kid: "k1", EC: newECKey(t)}},
	}, withClock(clock))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Rotate(SigningKey{Kid: "k2", EC: newECKey(t)}); err != nil {
		t.Fatal(err)
	}

	// Within the window: retiring k1 is still published.
	if !jwkKids(p.PublicJWKS())["k1"] {
		t.Fatal("retiring key dropped from JWKS while inside overlap window")
	}

	// Advance past the window: retiring k1 leaves the JWKS (read-time guard),
	// active k2 remains.
	now = now.Add(2 * time.Hour)
	kids := jwkKids(p.PublicJWKS())
	if kids["k1"] {
		t.Fatal("retiring key still published past the overlap window")
	}
	if !kids["k2"] {
		t.Fatal("active key missing from JWKS")
	}
}

// TestNewProvider_SweepsRetiringKeyByPersistedInstant proves a retiring key's
// supplied retirement instant is honored: a key retired longer ago than the
// overlap window is swept immediately, while a freshly-retiring key (zero instant
// → "now") stays published. This is the in-memory half of the round-trip fix that
// keeps a reloaded key from being treated as freshly retired forever.
func TestNewProvider_SweepsRetiringKeyByPersistedInstant(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	p, err := newProvider(ProviderConfig{
		OverlapWindow: time.Hour,
		Keys: []SigningKey{
			{Kid: "active", EC: newECKey(t)},
			{Kid: "old-retiring", State: keyring.KeyStateRetiring, RetiredAt: now.Add(-2 * time.Hour), EC: newECKey(t)},
		},
	}, withClock(clock))
	if err != nil {
		t.Fatal(err)
	}
	if jwkKids(p.PublicJWKS())["old-retiring"] {
		t.Fatal("retiring key with a 2h-old retiredAt should be swept (overlap 1h)")
	}

	p2, err := newProvider(ProviderConfig{
		OverlapWindow: time.Hour,
		Keys: []SigningKey{
			{Kid: "active", EC: newECKey(t)},
			{Kid: "fresh-retiring", State: keyring.KeyStateRetiring, EC: newECKey(t)},
		},
	}, withClock(clock))
	if err != nil {
		t.Fatal(err)
	}
	if !jwkKids(p2.PublicJWKS())["fresh-retiring"] {
		t.Fatal("freshly-retiring key (zero instant → now) should remain published within overlap")
	}
}

// TestProviderFromStore_HonorsPersistedRetiredAt proves the retirement instant
// survives the KeyringStore seam: a retiring key persisted with a long-past
// retiredAt is swept from the JWKS on load rather than republished as if
// freshly retired.
func TestProviderFromStore_HonorsPersistedRetiredAt(t *testing.T) {
	kr := loadKeyringsFixture(t)
	found := false
	for i := range kr.Keys {
		if kr.Keys[i].State == keyring.KeyStateRetiring {
			// Retired far beyond any sane overlap window.
			kr.Keys[i].RetiredAt = time.Now().Add(-100 * 365 * 24 * time.Hour).UTC().Format(time.RFC3339Nano)
			found = true
		}
	}
	if !found {
		t.Fatal("fixture has no retiring key to stamp")
	}
	store := NewInMemoryKeyringStore(kr)
	p, err := NewSigningKeyProviderFromStore(context.Background(), store, ProviderConfig{})
	if err != nil {
		t.Fatalf("provider from store: %v", err)
	}
	kids := jwkKids(p.PublicJWKS())
	if kids["rsa-retiring"] {
		t.Fatal("long-retired key still published: persisted retiredAt not honored across the store seam")
	}
	if !kids["ec-active"] {
		t.Fatal("active key missing from JWKS")
	}
}

// TestProviderFromStore_AllowEphemeralOnEmptyStore proves AllowEphemeral is
// consistent across both constructors: an empty store fails closed by default but
// mints a throwaway signer when the caller opts in — the serverless cold-start
// case.
func TestProviderFromStore_AllowEphemeralOnEmptyStore(t *testing.T) {
	empty := NewInMemoryKeyringStore(nil)
	if _, err := NewSigningKeyProviderFromStore(context.Background(), empty, ProviderConfig{}); !errors.Is(err, ErrNoSigningKey) {
		t.Fatalf("empty store default = %v, want ErrNoSigningKey", err)
	}
	p, err := NewSigningKeyProviderFromStore(context.Background(), empty, ProviderConfig{AllowEphemeral: true})
	if err != nil {
		t.Fatalf("empty store with AllowEphemeral: %v", err)
	}
	if p.ActiveKID() == "" {
		t.Fatal("ephemeral provider has no active key")
	}
	if _, err := p.Sign(standardClaims()); err != nil {
		t.Fatalf("ephemeral provider cannot sign: %v", err)
	}
}

// TestSigningKeyProvider_OverlapWindowExposed proves OverlapWindow is part of the
// interface, so JWKSHandler clamps its cache TTL to the provider's REAL overlap
// rather than falling back to a default for a provider it cannot type-assert.
func TestSigningKeyProvider_OverlapWindowExposed(t *testing.T) {
	p, err := NewSigningKeyProvider(ProviderConfig{
		OverlapWindow: 2 * time.Hour,
		Keys:          []SigningKey{ecFixtureSigningKey(t)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := p.OverlapWindow(); got != 2*time.Hour {
		t.Fatalf("OverlapWindow() = %v, want 2h", got)
	}
	handler := JWKSHandler(p, JWKSHandlerConfig{MaxAge: 10 * time.Hour})
	srv := httptest.NewServer(handler)
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck // test cleanup
	if maxAge := parseMaxAge(t, resp.Header.Get("Cache-Control")); maxAge > int((2 * time.Hour).Seconds()) {
		t.Fatalf("max-age=%d exceeds the provider's 2h overlap", maxAge)
	}
}

// --- Concurrency: atomic active-key switch under -race ---

func TestRotate_ConcurrentSignAndRotate(t *testing.T) {
	// A large overlap keeps every rotated-out key published for the test's
	// duration, so a token signed at any instant still verifies against a later
	// snapshot.
	p, err := NewSigningKeyProvider(ProviderConfig{
		OverlapWindow: time.Hour,
		Keys:          []SigningKey{{Kid: "gen-0", EC: newECKey(t)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ip := p.(*inMemoryProvider)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Rotator.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 1; i <= 15; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if err := ip.Rotate(SigningKey{Kid: "gen-" + strconv.Itoa(i), EC: newECKey(t)}); err != nil {
				t.Errorf("rotate: %v", err)
				return
			}
		}
	}()

	// Signers.
	for g := 0; g < 6; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 30; i++ {
				token, err := p.Sign(standardClaims())
				if err != nil {
					t.Errorf("concurrent sign: %v", err)
					return
				}
				if p.ActiveKID() == "" {
					t.Error("observed empty active kid mid-rotation")
					return
				}
				if n := snapshotActiveCount(t, p); n != 1 {
					t.Errorf("observed %d active keys mid-rotation, want exactly 1", n)
					return
				}
				// The signed token verifies against the current JWKS (its kid is still
				// published within the large overlap window).
				verifyThroughExistingPath(t, p.PublicJWKS(), token, "", "")
			}
		}()
	}

	wg.Wait()
	close(stop)
}

// --- Fail-closed ephemeral policy ---

func TestEphemeral_FailsClosedByDefault(t *testing.T) {
	_, err := NewSigningKeyProvider(ProviderConfig{}) // no keys, AllowEphemeral false
	if !errors.Is(err, ErrNoSigningKey) {
		t.Fatalf("no-key construction error = %v, want ErrNoSigningKey", err)
	}
}

func TestEphemeral_OptInGeneratesActiveKey(t *testing.T) {
	p, err := NewSigningKeyProvider(ProviderConfig{AllowEphemeral: true})
	if err != nil {
		t.Fatalf("ephemeral opt-in construction: %v", err)
	}
	if p.ActiveKID() == "" {
		t.Fatal("ephemeral provider has no active kid")
	}
	if !strings.HasPrefix(p.ActiveKID(), "ephemeral-") {
		t.Fatalf("ephemeral kid = %q, want ephemeral- prefix", p.ActiveKID())
	}
	token, err := p.Sign(standardClaims())
	if err != nil {
		t.Fatalf("ephemeral sign: %v", err)
	}
	verifyThroughExistingPath(t, p.PublicJWKS(), token, "putnami", "https://issuer.example")
}

// --- JWKS publication handler ---

func TestJWKSHandler_ServesConsumableJWKSWithBoundedCache(t *testing.T) {
	overlap := 300 * time.Second
	p, err := NewSigningKeyProvider(ProviderConfig{
		OverlapWindow: overlap,
		Keys:          []SigningKey{ecFixtureSigningKey(t)},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Request a max-age larger than the overlap; the handler must clamp it.
	handler := JWKSHandler(p, JWKSHandlerConfig{MaxAge: time.Hour})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	// Cache-Control max-age must be ≤ the overlap window (overlap ≥ consumer TTL).
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck // test cleanup
	cc := resp.Header.Get("Cache-Control")
	maxAge := parseMaxAge(t, cc)
	if maxAge > int(overlap.Seconds()) {
		t.Fatalf("Cache-Control max-age=%d exceeds overlap window %d", maxAge, int(overlap.Seconds()))
	}
	if resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("content-type = %q", resp.Header.Get("Content-Type"))
	}

	// The served document is consumed by the real JWKSFetcher and verifies a token
	// signed by the provider — the unknown-kid refetch path finds the active key.
	token, err := p.Sign(standardClaims())
	if err != nil {
		t.Fatal(err)
	}
	fetcher := NewJWKSFetcher(srv.URL, time.Minute, false)
	if _, err := validateJWKS(token, fetcher, "putnami", "https://issuer.example", true); err != nil {
		t.Fatalf("token from provider failed to verify via the served JWKS endpoint: %v", err)
	}
}

func parseMaxAge(t *testing.T, cacheControl string) int {
	t.Helper()
	for _, part := range strings.Split(cacheControl, ",") {
		part = strings.TrimSpace(part)
		if v, ok := strings.CutPrefix(part, "max-age="); ok {
			n, err := strconv.Atoi(v)
			if err != nil {
				t.Fatalf("bad max-age %q: %v", v, err)
			}
			return n
		}
	}
	t.Fatalf("no max-age in Cache-Control %q", cacheControl)
	return 0
}

// --- Redaction / safety ---

// TestPublicJWKS_NeverContainsPrivateMaterial asserts, on the serialized bytes,
// that the published JWKS carries none of the private JWK field names, for both
// EC and RSA active keys.
func TestPublicJWKS_NeverContainsPrivateMaterial(t *testing.T) {
	spectest.Proves(t, "go/authentication-authorization", "secret-non-disclosure", "public-jwks-never-contains-private-material")
	cases := []struct {
		name string
		key  SigningKey
	}{
		{"ec", ecFixtureSigningKey(t)},
		{"rsa", rsaFixtureSigningKey(t)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := NewSigningKeyProvider(ProviderConfig{Keys: []SigningKey{tc.key}})
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(p.PublicJWKS())
			if err != nil {
				t.Fatal(err)
			}
			assertNoPrivateFields(t, raw)
		})
	}
}

// TestPublicJWKS_ReusesKeyringProjectionGuarantee feeds a private keyring that
// DOES carry private material (the private keyrings fixture) through
// keyring.PublicJWKS and asserts the projection both matches the fixture's
// expected public set and drops every private field — the guarantee the provider
// leans on.
func TestPublicJWKS_ReusesKeyringProjectionGuarantee(t *testing.T) {
	kr := loadKeyringsFixture(t)
	// The fixture private keyring carries d/p/q/... material.
	privRaw, err := json.Marshal(kr)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(privRaw), `"d"`) {
		t.Fatal("test precondition failed: fixture keyring should carry private material")
	}
	pub := keyring.PublicJWKS(kr)
	pubRaw, err := json.Marshal(pub)
	if err != nil {
		t.Fatal(err)
	}
	assertNoPrivateFields(t, pubRaw)
	// active + retiring survive; oct/revoked/expired are dropped.
	kids := jwkKids(pub)
	if !kids["ec-active"] || !kids["rsa-retiring"] {
		t.Fatalf("projection kids = %v, want ec-active and rsa-retiring", kids)
	}
	if kids["hmac-active"] || kids["ec-revoked"] || kids["ec-expired"] {
		t.Fatalf("projection published a distrusted/symmetric key: %v", kids)
	}
}

func assertNoPrivateFields(t *testing.T, jwksJSON []byte) {
	t.Helper()
	var doc struct {
		Keys []map[string]json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(jwksJSON, &doc); err != nil {
		t.Fatalf("unmarshal jwks: %v", err)
	}
	private := []string{"d", "p", "q", "dp", "dq", "qi", "k"}
	for _, key := range doc.Keys {
		for _, f := range private {
			if _, ok := key[f]; ok {
				t.Errorf("published JWK leaks private field %q: %s", f, jwksJSON)
			}
		}
	}
}

// TestManagedKey_StringRedactsPrivateMaterial asserts a key's String never
// includes its private scalar, so it is safe to log or embed in an error.
func TestManagedKey_StringRedactsPrivateMaterial(t *testing.T) {
	spectest.Proves(t, "go/authentication-authorization", "secret-non-disclosure", "managed-key-string-redacts-private-material")
	ec := newECKey(t)
	mk, err := newManagedKey(SigningKey{Kid: "k1", EC: ec})
	if err != nil {
		t.Fatal(err)
	}
	s := mk.String()
	if !strings.Contains(s, "k1") || strings.Contains(s, b64u(ec.D.Bytes())) {
		t.Fatalf("String output leaks or mislabels: %q", s)
	}
}

// TestSignSource_NoLogging is a structural guard that the sign-side code never
// logs, so private key material cannot escape through a log line.
func TestSignSource_NoLogging(t *testing.T) {
	spectest.Proves(t, "go/authentication-authorization", "secret-non-disclosure", "sign-source-emits-no-log")
	for _, f := range []string{"keyring.go", "sign.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(src), "slog.") || strings.Contains(string(src), "\"log/slog\"") {
			t.Errorf("%s must not log (private key material must never reach a log line)", f)
		}
	}
}

// --- Keyring store ---

func TestInMemoryKeyringStore_SaveLoadRoundTrip(t *testing.T) {
	kr := loadKeyringsFixture(t)
	store := NewInMemoryKeyringStore(nil)

	if _, err := store.Load(context.Background()); !errors.Is(err, ErrNoSigningKey) {
		t.Fatalf("empty store Load = %v, want ErrNoSigningKey", err)
	}
	if err := store.Save(context.Background(), kr); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Keys) != len(kr.Keys) {
		t.Fatalf("loaded %d keys, want %d", len(loaded.Keys), len(kr.Keys))
	}
	// Mutating the loaded copy must not affect the store (deep copy).
	loaded.Keys = loaded.Keys[:0]
	again, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Keys) != len(kr.Keys) {
		t.Fatal("store mutated through a returned pointer")
	}
}

func TestProviderFromStore_SignsAndVerifies(t *testing.T) {
	// The fixture keyring has exactly one active (ec-active) and one retiring
	// (rsa-retiring) publishable key; oct/revoked/expired are skipped on load.
	store := NewInMemoryKeyringStore(loadKeyringsFixture(t))
	p, err := NewSigningKeyProviderFromStore(context.Background(), store, ProviderConfig{})
	if err != nil {
		t.Fatalf("provider from store: %v", err)
	}
	if p.ActiveKID() != "ec-active" {
		t.Fatalf("active kid = %q, want ec-active", p.ActiveKID())
	}
	// Both publishable keys are advertised so a verifier that refetches on an
	// unknown kid finds the retiring key too.
	if kids := jwkKids(p.PublicJWKS()); !kids["ec-active"] || !kids["rsa-retiring"] {
		t.Fatalf("store-loaded JWKS kids = %v", kids)
	}
	token, err := p.Sign(standardClaims())
	if err != nil {
		t.Fatal(err)
	}
	verifyThroughExistingPath(t, p.PublicJWKS(), token, "putnami", "https://issuer.example")
}

// --- Revocation (fail-closed publication) ---

func TestRevoke_RemovesKeyFromJWKSAndProtectsActive(t *testing.T) {
	spectest.Proves(t, "go/authentication-authorization", "jwks-overlap-and-revocation", "revoked-key-is-removed-from-the-jwks")
	p, err := NewSigningKeyProvider(ProviderConfig{
		OverlapWindow: time.Hour,
		Keys:          []SigningKey{{Kid: "k1", EC: newECKey(t)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ip := p.(*inMemoryProvider)
	if err := ip.Rotate(SigningKey{Kid: "k2", EC: newECKey(t)}); err != nil {
		t.Fatal(err)
	}
	// The active key cannot be revoked directly (would leave zero active).
	if err := ip.Revoke("k2"); err == nil {
		t.Fatal("expected error revoking the active key")
	}
	// Revoking the retiring key removes it from the JWKS (fail-closed publication).
	if err := ip.Revoke("k1"); err != nil {
		t.Fatalf("revoke retiring key: %v", err)
	}
	if jwkKids(p.PublicJWKS())["k1"] {
		t.Fatal("revoked key still published in JWKS")
	}
	if snapshotActiveCount(t, p) != 1 {
		t.Fatal("revocation disturbed the single active key")
	}
}

// --- Key-state fixture: rotation-relevant transitions ---

// TestKeyStateFixture_RotationTransitions exercises the state-machine cases the
// rotation/revocation code depends on against the shared corpus, so a
// change to the transition table that would break rotation is caught here too.
func TestKeyStateFixture_RotationTransitions(t *testing.T) {
	data, err := os.ReadFile(keyStatesPath)
	if err != nil {
		t.Fatalf("read key-states fixture: %v", err)
	}
	var file struct {
		Transitions []struct {
			ID    string           `json:"id"`
			From  keyring.KeyState `json:"from"`
			To    keyring.KeyState `json:"to"`
			Legal bool             `json:"legal"`
		} `json:"transitions"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatalf("parse key-states fixture: %v", err)
	}
	// Transitions the sign side relies on: active→retiring (rotation demote) and
	// the terminal revoked reachability; retiring→active must stay illegal.
	relevant := map[string]bool{
		"active-to-retiring":  true,
		"retiring-to-expired": true,
		"retiring-to-revoked": true,
		"retiring-to-active":  true,
		"active-to-revoked":   true,
	}
	seen := 0
	for _, tc := range file.Transitions {
		if !relevant[tc.ID] {
			continue
		}
		seen++
		if got := keyring.CanTransition(tc.From, tc.To); got != tc.Legal {
			t.Errorf("%s: CanTransition(%s,%s)=%t, fixture says %t", tc.ID, tc.From, tc.To, got, tc.Legal)
		}
	}
	if seen == 0 {
		t.Fatal("no relevant transition cases found in fixture")
	}
}
