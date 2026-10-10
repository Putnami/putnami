package runtime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

func TestEnvVarTokenSource(t *testing.T) {
	t.Run("returns env value", func(t *testing.T) {
		t.Setenv("CONFIG_TEST_TOKEN", "abc123")
		ts := NewEnvVarTokenSource("CONFIG_TEST_TOKEN")
		got, err := ts.Token(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "abc123" {
			t.Errorf("expected abc123, got %q", got)
		}
	})

	t.Run("errors when unset", func(t *testing.T) {
		t.Setenv("CONFIG_TEST_TOKEN", "")
		ts := NewEnvVarTokenSource("CONFIG_TEST_TOKEN")
		if _, err := ts.Token(context.Background()); err == nil {
			t.Fatal("expected error when env unset, got nil")
		}
	})

	t.Run("errors when name empty", func(t *testing.T) {
		ts := NewEnvVarTokenSource("")
		if _, err := ts.Token(context.Background()); err == nil {
			t.Fatal("expected error when name empty, got nil")
		}
	})

	t.Run("trims whitespace", func(t *testing.T) {
		t.Setenv("CONFIG_TEST_TOKEN", "  spaced  ")
		ts := NewEnvVarTokenSource("CONFIG_TEST_TOKEN")
		got, _ := ts.Token(context.Background())
		if got != "spaced" {
			t.Errorf("expected trimmed value, got %q", got)
		}
	})
}

// jwtWithExp builds a minimal unsigned JWT carrying the given exp claim,
// used so tests can drive the metadata-token cache without a real signer.
func jwtWithExp(exp int64) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload, _ := json.Marshal(map[string]int64{"exp": exp})
	payloadEnc := base64.RawURLEncoding.EncodeToString(payload)
	return header + "." + payloadEnc + ".signature"
}

func TestGcpMetadataTokenSource_Token(t *testing.T) {
	var hits int32
	var gotAudience, gotFlavor, gotFormat string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		q := r.URL.Query()
		gotAudience = q.Get("audience")
		gotFormat = q.Get("format")
		gotFlavor = r.Header.Get("Metadata-Flavor")
		_, _ = fmt.Fprint(w, jwtWithExp(time.Now().Add(time.Hour).Unix()))
	}))
	defer server.Close()

	// Redirect metadata.google.internal traffic to the test server —
	// the URL constant is package-private, so we route at the transport
	// layer instead.
	src := &GcpMetadataTokenSource{
		Audience: "https://control.putnami.test",
		Client: &http.Client{
			Transport: redirectTransport(server.URL),
			Timeout:   2 * time.Second,
		},
	}

	tok, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok == "" {
		t.Fatal("expected token, got empty")
	}
	if gotAudience != "https://control.putnami.test" {
		t.Errorf("expected audience forwarded, got %q", gotAudience)
	}
	if gotFormat != "full" {
		t.Errorf("expected format=full, got %q", gotFormat)
	}
	if gotFlavor != "Google" {
		t.Errorf("expected Metadata-Flavor=Google, got %q", gotFlavor)
	}

	// Second call within TTL must be cached (no extra server hit).
	if _, err := src.Token(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("expected 1 server hit (cached), got %d", got)
	}

	// Forcing the expiry into the past triggers a refetch.
	src.mu.Lock()
	src.expiry = time.Now().Add(-time.Minute)
	src.mu.Unlock()
	if _, err := src.Token(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Errorf("expected 2 server hits after forced expiry, got %d", got)
	}
}

func TestGcpMetadataTokenSource_AudienceRequired(t *testing.T) {
	src := &GcpMetadataTokenSource{}
	if _, err := src.Token(context.Background()); err == nil {
		t.Fatal("expected error when audience empty, got nil")
	}
}

func TestGcpMetadataTokenSource_Non200(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	src := &GcpMetadataTokenSource{
		Audience: "https://control.putnami.test",
		Client: &http.Client{
			Transport: redirectTransport(server.URL),
			Timeout:   2 * time.Second,
		},
	}
	if _, err := src.Token(context.Background()); err == nil {
		t.Fatal("expected error on 500, got nil")
	}
}

func TestGcpMetadataTokenSource_EmptyBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	src := &GcpMetadataTokenSource{
		Audience: "https://control.putnami.test",
		Client: &http.Client{
			Transport: redirectTransport(server.URL),
			Timeout:   2 * time.Second,
		},
	}
	if _, err := src.Token(context.Background()); err == nil {
		t.Fatal("expected error on empty body, got nil")
	}
}

func TestGcpMetadataTokenSource_ParseExpiry(t *testing.T) {
	exp := time.Now().Add(45 * time.Minute).Unix()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, jwtWithExp(exp))
	}))
	defer server.Close()

	src := &GcpMetadataTokenSource{
		Audience: "https://control.putnami.test",
		Client: &http.Client{
			Transport: redirectTransport(server.URL),
			Timeout:   2 * time.Second,
		},
	}
	if _, err := src.Token(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	src.mu.Lock()
	got := src.expiry.Unix()
	src.mu.Unlock()
	if got != exp {
		t.Errorf("expected expiry parsed from JWT (%d), got %d", exp, got)
	}
}

func TestParseJWTExpiry(t *testing.T) {
	t.Run("valid token", func(t *testing.T) {
		exp := time.Now().Add(time.Hour).Unix()
		got := parseJWTExpiry(jwtWithExp(exp))
		if got.Unix() != exp {
			t.Errorf("expected exp=%d, got %d", exp, got.Unix())
		}
	})

	t.Run("missing exp returns zero", func(t *testing.T) {
		header := base64.RawURLEncoding.EncodeToString([]byte(`{}`))
		payload := base64.RawURLEncoding.EncodeToString([]byte(`{}`))
		tok := header + "." + payload + ".sig"
		if got := parseJWTExpiry(tok); !got.IsZero() {
			t.Errorf("expected zero time, got %v", got)
		}
	})

	t.Run("malformed returns zero", func(t *testing.T) {
		if got := parseJWTExpiry("not-a-jwt"); !got.IsZero() {
			t.Errorf("expected zero time, got %v", got)
		}
	})
}

func TestMetadataServerReachable_FalseOffGCP(t *testing.T) {
	// Off-GCP runs (CI, local dev) have no metadata.google.internal in
	// DNS, so the probe should fail fast. Guard against flake by giving
	// it 500ms — DNS failure is typically <10ms.
	if MetadataServerReachable(500 * time.Millisecond) {
		t.Skip("environment unexpectedly reaches metadata.google.internal; skipping")
	}
}

// redirectTransport rewrites every outbound request to point at the
// given test-server URL while preserving the path and query — lets us
// exercise the metadata code path without resolving
// metadata.google.internal.
func redirectTransport(target string) http.RoundTripper {
	parsed, err := url.Parse(target)
	if err != nil {
		panic(err)
	}
	return roundTripFn(func(r *http.Request) (*http.Response, error) {
		r2 := r.Clone(r.Context())
		r2.URL.Scheme = parsed.Scheme
		r2.URL.Host = parsed.Host
		r2.Host = parsed.Host
		return http.DefaultTransport.RoundTrip(r2)
	})
}

type roundTripFn func(*http.Request) (*http.Response, error)

func (f roundTripFn) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
