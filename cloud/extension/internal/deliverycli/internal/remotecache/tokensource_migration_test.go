package remotecache

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"

	cache "go.putnami.dev/protocol/cache"
)

// TestMigrateLegacyTokenCommand: the superseded combined --for-cache flag is
// rewritten to the canonical --for cache; canonical and third-party/custom
// commands pass through untouched.
func TestMigrateLegacyTokenCommand(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{
			"legacy --for-cache migrates to --for cache",
			[]string{"putnami", "cloud", "token", "--for-cache"},
			[]string{"putnami", "cloud", "token", "--for", "cache"},
		},
		{
			"canonical form unchanged",
			[]string{"putnami", "cloud", "token", "--for", "cache"},
			[]string{"putnami", "cloud", "token", "--for", "cache"},
		},
		{
			"third-party/custom command preserved",
			[]string{"my-token-helper", "--scope", "cache"},
			[]string{"my-token-helper", "--scope", "cache"},
		},
		{"nil unchanged", nil, nil},
		{"empty unchanged", []string{}, []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := migrateLegacyTokenCommand(tt.in); !slices.Equal(got, tt.want) {
				t.Errorf("migrateLegacyTokenCommand(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// TestLoadConfig_MigratesLegacyForCacheCommand: a persisted cache.json carrying
// the old --for-cache recipe is migrated on read, and the rest of the config
// (URL) is preserved.
func TestLoadConfig_MigratesLegacyForCacheCommand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")
	body := `{"url":"https://cache.putnami.cloud","token":{"command":["putnami","cloud","token","--for-cache"]}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	want := []string{"putnami", "cloud", "token", "--for", "cache"}
	if !slices.Equal(cfg.Token.Command, want) {
		t.Errorf("migrated command = %v, want %v", cfg.Token.Command, want)
	}
	if cfg.URL != "https://cache.putnami.cloud" {
		t.Errorf("URL = %q, want it preserved", cfg.URL)
	}
}

// TestLoadConfig_PreservesThirdPartyCommand: a non-Putnami token recipe is read
// back verbatim (no migration, no rewrite).
func TestLoadConfig_PreservesThirdPartyCommand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")
	body := `{"url":"https://cache.example.com","token":{"command":["corp-cache-token","--audience","build"]}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	want := []string{"corp-cache-token", "--audience", "build"}
	if !slices.Equal(cfg.Token.Command, want) {
		t.Errorf("third-party command = %v, want it preserved as %v", cfg.Token.Command, want)
	}
	if cfg.URL != "https://cache.example.com" {
		t.Errorf("URL = %q, want it preserved", cfg.URL)
	}
}

// TestTokenSource_CommandTrimmed: the bearer is the command's stdout with
// surrounding whitespace trimmed.
func TestTokenSource_CommandTrimmed(t *testing.T) {
	t.Setenv(TokenEnv, "")
	got, err := TokenSource{Command: []string{"sh", "-c", "printf '  cache-bearer\n'"}}.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "cache-bearer" {
		t.Errorf("token = %q, want cache-bearer (trimmed)", got)
	}
}

// TestClient_BearerLazyAndMemoized: the token source runs only when a request
// needs a bearer — never at construction, never for a no-op negotiate — and is
// resolved at most once per session (memoized) and sent on the wire.
func TestClient_BearerLazyAndMemoized(t *testing.T) {
	t.Setenv(TokenEnv, "")
	var calls int32
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get(cache.AuthorizationHeader)
		// 404 → Capabilities reports an empty set without error; the request (and
		// its Authorization header) still reached the server.
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "", WithHTTPClient(srv.Client()), WithTokenFunc(func(context.Context) (string, error) {
		atomic.AddInt32(&calls, 1)
		return "lazy-bearer", nil
	}))

	if n := atomic.LoadInt32(&calls); n != 0 {
		t.Fatalf("token resolved %d times at construction, want 0 (lazy)", n)
	}

	// A negotiate with no eligible keys short-circuits without a round trip, so it
	// must not resolve a bearer.
	if _, err := c.Negotiate(context.Background(), &cache.NegotiateRequest{}); err != nil {
		t.Fatalf("Negotiate(empty): %v", err)
	}
	if n := atomic.LoadInt32(&calls); n != 0 {
		t.Fatalf("token resolved %d times for a no-op negotiate, want 0", n)
	}

	// Two real requests → resolved exactly once (memoized), bearer sent.
	if _, err := c.Capabilities(context.Background()); err != nil {
		t.Fatalf("Capabilities #1: %v", err)
	}
	if _, err := c.Capabilities(context.Background()); err != nil {
		t.Fatalf("Capabilities #2: %v", err)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("token resolved %d times across two requests, want 1 (memoized)", n)
	}
	if gotAuth != "Bearer lazy-bearer" {
		t.Errorf("auth header = %q, want Bearer lazy-bearer", gotAuth)
	}
}
