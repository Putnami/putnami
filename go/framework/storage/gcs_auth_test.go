package storage

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// generateKeyPEM returns a fresh RSA key and its PKCS#8 PEM encoding.
func generateKeyPEM(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey: %v", err)
	}
	return key, string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func TestParseServiceAccountKey(t *testing.T) {
	_, keyPEM := generateKeyPEM(t)
	saJSON, _ := json.Marshal(map[string]string{
		"type":         "service_account",
		"client_email": "svc@proj.iam.gserviceaccount.com",
		"private_key":  keyPEM,
		"token_uri":    "https://oauth2.googleapis.com/token",
	})

	key, signKey, err := parseServiceAccountKey(saJSON)
	if err != nil {
		t.Fatalf("parseServiceAccountKey: %v", err)
	}
	if key.ClientEmail != "svc@proj.iam.gserviceaccount.com" {
		t.Errorf("ClientEmail = %q", key.ClientEmail)
	}
	if signKey == nil {
		t.Fatal("expected non-nil signing key")
	}
}

func TestParseServiceAccountKey_BadJSON(t *testing.T) {
	if _, _, err := parseServiceAccountKey([]byte("not json")); err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestParseServiceAccountKey_BadPEM(t *testing.T) {
	saJSON, _ := json.Marshal(map[string]string{
		"type":        "service_account",
		"private_key": "-----BEGIN PRIVATE KEY-----\nnotbase64\n-----END PRIVATE KEY-----",
	})
	if _, _, err := parseServiceAccountKey(saJSON); err == nil {
		t.Fatal("expected error for invalid PEM body")
	}
}

func TestBuildJWTAssertion(t *testing.T) {
	signKey, _ := generateKeyPEM(t)
	key := &serviceAccountKey{
		ClientEmail:  "svc@proj.iam.gserviceaccount.com",
		TokenURI:     "https://oauth2.googleapis.com/token",
		PrivateKeyID: "key-id-1",
	}

	assertion, err := buildJWTAssertion(key, signKey, time.Unix(1_700_000_000, 0))
	if err != nil {
		t.Fatalf("buildJWTAssertion: %v", err)
	}
	parts := strings.Split(assertion, ".")
	if len(parts) != 3 {
		t.Fatalf("expected 3 JWT parts, got %d", len(parts))
	}

	headerJSON, _ := base64.RawURLEncoding.DecodeString(parts[0])
	var header map[string]string
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		t.Fatalf("decode header: %v", err)
	}
	if header["alg"] != "RS256" || header["kid"] != "key-id-1" {
		t.Errorf("unexpected header %v", header)
	}

	claimsJSON, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims map[string]any
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		t.Fatalf("decode claims: %v", err)
	}
	if claims["iss"] != "svc@proj.iam.gserviceaccount.com" {
		t.Errorf("iss = %v", claims["iss"])
	}
	if claims["scope"] != gcsReadWriteScope {
		t.Errorf("scope = %v", claims["scope"])
	}

	// The signature must verify against the public key.
	signingInput := parts[0] + "." + parts[1]
	digest := sha256.Sum256([]byte(signingInput))
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	if err := rsa.VerifyPKCS1v15(&signKey.PublicKey, crypto.SHA256, digest[:], sig); err != nil {
		t.Errorf("signature does not verify: %v", err)
	}
}

func TestMetadataTokenSource(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Metadata-Flavor") != "Google" {
			t.Errorf("missing Metadata-Flavor header")
		}
		if r.URL.Path != gcsMetadataTokenPath {
			t.Errorf("path = %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "meta-token", "expires_in": 3600})
	}))
	defer srv.Close()

	ts := newMetadataTokenSource(srv.Client(), srv.URL)
	tok, err := ts.token(context.Background())
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	if tok != "meta-token" {
		t.Errorf("token = %q, want meta-token", tok)
	}

	// A second call within the validity window is served from cache.
	if _, err := ts.token(context.Background()); err != nil {
		t.Fatalf("token (cached): %v", err)
	}
	if calls.Load() != 1 {
		t.Errorf("expected 1 upstream fetch, got %d", calls.Load())
	}
}

func TestMetadataTokenSource_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ts := newMetadataTokenSource(srv.Client(), srv.URL)
	if _, err := ts.token(context.Background()); err == nil {
		t.Fatal("expected error on metadata failure")
	}
}

func TestServiceAccountTokenSource(t *testing.T) {
	signKey, _ := generateKeyPEM(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm: %v", err)
		}
		if r.PostForm.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
			t.Errorf("grant_type = %q", r.PostForm.Get("grant_type"))
		}
		if r.PostForm.Get("assertion") == "" {
			t.Error("missing assertion")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "sa-token", "expires_in": 3600})
	}))
	defer srv.Close()

	key := &serviceAccountKey{ClientEmail: "svc@proj.iam.gserviceaccount.com", TokenURI: srv.URL}
	ts := newServiceAccountTokenSource(srv.Client(), key, signKey)
	tok, err := ts.token(context.Background())
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	if tok != "sa-token" {
		t.Errorf("token = %q, want sa-token", tok)
	}
}

func TestGCSBackend_authorize(t *testing.T) {
	t.Run("attaches bearer token", func(t *testing.T) {
		b := &GCSBackend{tokens: staticTokenSource("abc123")}
		req, _ := http.NewRequest(http.MethodGet, "https://storage.googleapis.com/", nil)
		if err := b.authorize(context.Background(), req); err != nil {
			t.Fatalf("authorize: %v", err)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer abc123" {
			t.Errorf("Authorization = %q, want Bearer abc123", got)
		}
	})

	t.Run("nil token source leaves request unauthenticated", func(t *testing.T) {
		b := &GCSBackend{}
		req, _ := http.NewRequest(http.MethodGet, "https://storage.googleapis.com/", nil)
		if err := b.authorize(context.Background(), req); err != nil {
			t.Fatalf("authorize: %v", err)
		}
		if got := req.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization = %q, want empty", got)
		}
	})
}

func TestNewGCSBackend_WithCredentialsFile(t *testing.T) {
	_, keyPEM := generateKeyPEM(t)
	saJSON, _ := json.Marshal(map[string]string{
		"type":         "service_account",
		"client_email": "svc@proj.iam.gserviceaccount.com",
		"private_key":  keyPEM,
		"token_uri":    "https://oauth2.googleapis.com/token",
	})
	path := filepath.Join(t.TempDir(), "creds.json")
	if err := os.WriteFile(path, saJSON, 0o600); err != nil {
		t.Fatalf("write creds: %v", err)
	}

	b, err := NewGCSBackend(context.Background(), GCSConfig{CredentialsFile: path})
	if err != nil {
		t.Fatalf("NewGCSBackend: %v", err)
	}
	if b.signEmail != "svc@proj.iam.gserviceaccount.com" {
		t.Errorf("signEmail = %q", b.signEmail)
	}
	if b.signKey == nil {
		t.Error("expected a local signing key from the credentials file")
	}
	if b.tokens == nil {
		t.Error("expected a token source")
	}
}

func TestNewGCSBackend_ApplicationDefault(t *testing.T) {
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	b, err := NewGCSBackend(context.Background(), GCSConfig{})
	if err != nil {
		t.Fatalf("NewGCSBackend: %v", err)
	}
	if b.tokens == nil {
		t.Error("expected a metadata token source under ADC")
	}
	if b.signKey != nil {
		t.Error("expected no local signing key under ADC (keyless)")
	}
}

func TestGCSBackend_resolveSignerEmail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != gcsMetadataEmailPath {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte("default@proj.iam.gserviceaccount.com\n"))
	}))
	defer srv.Close()

	b := &GCSBackend{httpClient: srv.Client(), metadataHost: srv.URL}
	email, err := b.resolveSignerEmail(context.Background())
	if err != nil {
		t.Fatalf("resolveSignerEmail: %v", err)
	}
	if email != "default@proj.iam.gserviceaccount.com" {
		t.Errorf("email = %q", email)
	}
}

// TestGCSBackend_SignedGetURL_IAM exercises the keyless signing path: the
// backend has no private key and signs the V4 string-to-sign through a fake IAM
// SignBlob endpoint.
func TestGCSBackend_SignedGetURL_IAM(t *testing.T) {
	signKey, _ := generateKeyPEM(t)
	var gotAuth string
	iam := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if !strings.Contains(r.URL.Path, ":signBlob") {
			t.Errorf("unexpected IAM path %q", r.URL.Path)
		}
		var req struct {
			Payload string `json:"payload"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		payload, err := base64.StdEncoding.DecodeString(req.Payload)
		if err != nil {
			t.Errorf("decode payload: %v", err)
		}
		// Faithfully sign SHA-256(payload), as IAM SignBlob does.
		digest := sha256.Sum256(payload)
		sig, _ := rsa.SignPKCS1v15(rand.Reader, signKey, crypto.SHA256, digest[:])
		_ = json.NewEncoder(w).Encode(map[string]string{"signedBlob": base64.StdEncoding.EncodeToString(sig)})
	}))
	defer iam.Close()

	b := &GCSBackend{
		httpClient:  iam.Client(),
		endpoint:    gcsDefaultEndpoint,
		iamEndpoint: iam.URL,
		tokens:      staticTokenSource("oauth-token"),
		signEmail:   "default@proj.iam.gserviceaccount.com",
	}

	got, err := b.SignedGetURL(context.Background(), "mybucket", "path/key.txt", SignedURLOptions{Expiry: time.Hour})
	if err != nil {
		t.Fatalf("SignedGetURL (IAM): %v", err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	if u.Host != "storage.googleapis.com" {
		t.Errorf("host = %q", u.Host)
	}
	q := u.Query()
	if q.Get("X-Goog-Algorithm") != "GOOG4-RSA-SHA256" {
		t.Errorf("algorithm = %q", q.Get("X-Goog-Algorithm"))
	}
	if q.Get("X-Goog-Signature") == "" {
		t.Error("expected non-empty signature")
	}
	if !strings.Contains(q.Get("X-Goog-Credential"), "default@proj.iam.gserviceaccount.com") {
		t.Errorf("credential = %q", q.Get("X-Goog-Credential"))
	}
	if gotAuth != "Bearer oauth-token" {
		t.Errorf("IAM call Authorization = %q, want Bearer oauth-token", gotAuth)
	}
}
