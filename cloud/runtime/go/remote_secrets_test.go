package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRemoteSecretsSource(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/secrets/resolve" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"secrets": map[string]any{
				"database": map[string]any{
					"password": "shh",
				},
			},
			"resolved":    true,
			"schemaMatch": true,
		})
	}))
	defer ts.Close()

	source := NewRemoteSecretsSource(RemoteSecretsSourceConfig{
		ServerURL:   ts.URL,
		AppName:     "test-app",
		Environment: "production",
		Timeout:     2 * time.Second,
	})

	if source.Name() != "secrets-server" {
		t.Errorf("expected name=secrets-server, got %s", source.Name())
	}
	if source.Priority() != 55 {
		t.Errorf("expected priority=55, got %d", source.Priority())
	}

	data, err := source.Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if data == nil {
		t.Fatal("expected data, got nil")
	}

	db, ok := data["database"].(map[string]any)
	if !ok {
		t.Fatal("expected database block")
	}
	if db["password"] != "shh" {
		t.Errorf("expected password=shh, got %v", db["password"])
	}
}

func TestRemoteSecretsSource_SendsBearerToken(t *testing.T) {
	var gotAuth, gotHash string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		var body resolveSecretsRequest
		if data, err := io.ReadAll(r.Body); err == nil {
			_ = json.Unmarshal(data, &body)
			gotHash = body.SchemaHash
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"secrets":     map[string]any{},
			"resolved":    true,
			"schemaMatch": true,
		})
	}))
	defer ts.Close()

	source := NewRemoteSecretsSource(RemoteSecretsSourceConfig{
		ServerURL:   ts.URL,
		AppName:     "test-app",
		Environment: "production",
		SchemaHash:  "sha256:abcdef0123456789",
		Token:       "test-token",
	})
	if _, err := source.Load(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.HasPrefix(gotAuth, "Bearer ") {
		t.Errorf("expected Bearer Authorization header, got %q", gotAuth)
	}
	if !strings.Contains(gotAuth, "test-token") {
		t.Errorf("expected token in Authorization header, got %q", gotAuth)
	}
	if gotHash != "sha256:abcdef0123456789" {
		t.Errorf("expected schemaHash forwarded, got %q", gotHash)
	}
}

func TestRemoteSecretsSource_NotResolved(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"secrets":  map[string]any{},
			"resolved": false,
		})
	}))
	defer ts.Close()

	source := NewRemoteSecretsSource(RemoteSecretsSourceConfig{
		ServerURL:   ts.URL,
		AppName:     "unknown",
		Environment: "local",
	})
	data, err := source.Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if data != nil {
		t.Errorf("expected nil for unresolved secrets, got %v", data)
	}
}

func TestRemoteSecretsSourceRetriesRequiredUntilReady(t *testing.T) {
	calls := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			http.Error(w, "warming up", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"secrets":     map[string]any{"database": map[string]any{"password": "ready"}},
			"resolved":    true,
			"schemaMatch": true,
		})
	}))
	defer ts.Close()

	source := NewRemoteSecretsSource(RemoteSecretsSourceConfig{
		ServerURL:   ts.URL,
		AppName:     "test-app",
		Environment: "prod",
		Timeout:     100 * time.Millisecond,
		RetryBudget: 500 * time.Millisecond,
		Required:    true,
	})

	data, err := source.Load()
	if err != nil {
		t.Fatalf("unexpected error after retry: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 calls, got %d", calls)
	}
	db, ok := data["database"].(map[string]any)
	if !ok {
		t.Fatalf("expected database secrets block, got %#v", data)
	}
	if db["password"] != "ready" {
		t.Errorf("expected database.password=ready, got %#v", db["password"])
	}
}

// isolateDiscoveryEnv clears every signal discoverTokenSource looks at
// so tests start from "off-GCP, no operator override." Tests then
// explicitly set whichever signal they want to assert on. Without
// this, a self-hosted GCP runner would silently flip the discovery
// outcome (metadata server reachable, no env hints needed) and tests
// would pass for the wrong reason or fail outright.
func isolateDiscoveryEnv(t *testing.T, metadataReachable bool) {
	t.Helper()
	t.Setenv(PreparedBootBindingEnv, "")
	for _, name := range configServerTokenEnvNames {
		t.Setenv(name, "")
	}
	t.Setenv("CONFIG_SERVER_AUDIENCE", "")
	t.Setenv("K_SERVICE", "")
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")
	prev := metadataReachableOnce
	metadataReachableOnce = func() bool { return metadataReachable }
	t.Cleanup(func() { metadataReachableOnce = prev })
}

func TestDiscoverRemoteSource_ReadsToken(t *testing.T) {
	// Both endpoints share one identity; the auto-discovery path must
	// forward CONFIG_SERVER_TOKEN to RemoteConfigSource so config calls
	// aren't silently unauthenticated when secrets calls are.
	isolateDiscoveryEnv(t, false)
	t.Setenv("CONFIG_SERVER_URL", "https://config.putnami.test")
	t.Setenv("CONFIG_SERVER_TOKEN", "test-token")
	t.Setenv("APP_NAME", "task-api")

	src := DiscoverRemoteSource()
	if src == nil {
		t.Fatal("expected RemoteConfigSource, got nil")
	}
	if src.config.TokenSource == nil {
		t.Fatal("expected TokenSource to be wired, got nil")
	}
	tok, err := src.config.TokenSource.Token(context.Background())
	if err != nil {
		t.Fatalf("token source returned error: %v", err)
	}
	if tok != "test-token" {
		t.Errorf("expected token=test-token, got %q", tok)
	}
}

func TestDiscoverRemoteSource_ReadsTimeoutAndRetryBudget(t *testing.T) {
	isolateDiscoveryEnv(t, false)
	t.Setenv("CONFIG_SERVER_URL", "https://config.putnami.test")
	t.Setenv("CONFIG_SERVER_TOKEN", "test-token")
	t.Setenv("CONFIG_SERVER_TIMEOUT", "2s")
	t.Setenv("CONFIG_SERVER_RETRY_BUDGET", "45s")

	src := DiscoverRemoteSource()
	if src == nil {
		t.Fatal("expected RemoteConfigSource, got nil")
	}
	if src.config.Timeout != 2*time.Second {
		t.Errorf("expected timeout=2s, got %s", src.config.Timeout)
	}
	if src.config.RetryBudget != 45*time.Second {
		t.Errorf("expected retry budget=45s, got %s", src.config.RetryBudget)
	}
}

func TestDiscoverRemoteSource_DisablesRetriesWhenRetryBudgetIsZero(t *testing.T) {
	isolateDiscoveryEnv(t, false)
	t.Setenv("CONFIG_SERVER_URL", "https://config.putnami.test")
	t.Setenv("CONFIG_SERVER_TOKEN", "test-token")
	t.Setenv("CONFIG_SERVER_RETRY_BUDGET", "0")

	src := DiscoverRemoteSource()
	if src == nil {
		t.Fatal("expected RemoteConfigSource, got nil")
	}
	if src.config.RetryBudget >= 0 {
		t.Errorf("expected retry budget to disable retries, got %s", src.config.RetryBudget)
	}
}

func TestDiscoverRemoteSecretsSource_ReadsTimeoutAndRetryBudget(t *testing.T) {
	isolateDiscoveryEnv(t, false)
	t.Setenv("CONFIG_SERVER_URL", "https://config.putnami.test")
	t.Setenv("CONFIG_SERVER_TOKEN", "test-token")
	t.Setenv("CONFIG_SERVER_TIMEOUT", "1500ms")
	t.Setenv("CONFIG_SERVER_RETRY_BUDGET", "20s")

	src := DiscoverRemoteSecretsSource()
	if src == nil {
		t.Fatal("expected RemoteSecretsSource, got nil")
	}
	if src.config.Timeout != 1500*time.Millisecond {
		t.Errorf("expected timeout=1500ms, got %s", src.config.Timeout)
	}
	if src.config.RetryBudget != 20*time.Second {
		t.Errorf("expected retry budget=20s, got %s", src.config.RetryBudget)
	}
}

func TestDiscoverRemoteSecretsSource_ReadsToken(t *testing.T) {
	isolateDiscoveryEnv(t, false)
	t.Setenv("CONFIG_SERVER_URL", "https://config.putnami.test")
	t.Setenv("CONFIG_SERVER_TOKEN", "test-token")
	t.Setenv("APP_NAME", "task-api")

	src := DiscoverRemoteSecretsSource()
	if src == nil {
		t.Fatal("expected RemoteSecretsSource, got nil")
	}
	if src.config.TokenSource == nil {
		t.Fatal("expected TokenSource to be wired, got nil")
	}
	tok, err := src.config.TokenSource.Token(context.Background())
	if err != nil {
		t.Fatalf("token source returned error: %v", err)
	}
	if tok != "test-token" {
		t.Errorf("expected token=test-token, got %q", tok)
	}
}

func TestDiscoverRemoteSource_PrefersEnvVarOverGCP(t *testing.T) {
	// CONFIG_SERVER_TOKEN is the explicit operator override and must win even on GCP where the
	// metadata server is reachable.
	isolateDiscoveryEnv(t, true) // force probe positive too — env var must still win
	t.Setenv("CONFIG_SERVER_URL", "https://config.putnami.test")
	t.Setenv("CONFIG_SERVER_TOKEN", "operator-override")
	t.Setenv("K_SERVICE", "task-api") // pretend Cloud Run

	src := DiscoverRemoteSource()
	if src == nil {
		t.Fatal("expected RemoteConfigSource, got nil")
	}
	if _, ok := src.config.TokenSource.(*EnvVarTokenSource); !ok {
		t.Errorf("expected EnvVarTokenSource (env var wins per protocol), got %T", src.config.TokenSource)
	}
}

func TestDiscoverRemoteSource_PicksMetadataOnGCP(t *testing.T) {
	// On GCP without CONFIG_SERVER_TOKEN, workload identity kicks in via
	// the metadata server. K_SERVICE is the canonical signal.
	isolateDiscoveryEnv(t, false) // probe disabled — K_SERVICE alone must trigger GCP
	t.Setenv("CONFIG_SERVER_URL", "https://config.putnami.test")
	t.Setenv("K_SERVICE", "task-api")

	src := DiscoverRemoteSource()
	if src == nil {
		t.Fatal("expected RemoteConfigSource, got nil")
	}
	gcp, ok := src.config.TokenSource.(*GcpMetadataTokenSource)
	if !ok {
		t.Fatalf("expected GcpMetadataTokenSource on GCP, got %T", src.config.TokenSource)
	}
	if gcp.Audience != "https://config.putnami.test" {
		t.Errorf("expected audience to match CONFIG_SERVER_URL, got %q", gcp.Audience)
	}
}

func TestDiscoverRemoteSource_UsesConfiguredAudienceOnGCP(t *testing.T) {
	isolateDiscoveryEnv(t, false)
	t.Setenv("CONFIG_SERVER_URL", "https://config.example.test/api/configs/resolve?appName=shop%2Fserver&environment=prod&secretsMode=reveal")
	t.Setenv("CONFIG_SERVER_AUDIENCE", "https://config.example.test")
	t.Setenv("K_SERVICE", "auth-server")

	src := DiscoverRemoteSource()
	if src == nil {
		t.Fatal("expected RemoteConfigSource, got nil")
	}
	gcp, ok := src.config.TokenSource.(*GcpMetadataTokenSource)
	if !ok {
		t.Fatalf("expected GcpMetadataTokenSource on GCP, got %T", src.config.TokenSource)
	}
	if gcp.Audience != "https://config.example.test" {
		t.Errorf("audience = %q, want https://config.example.test", gcp.Audience)
	}
	if src.config.URL == "" {
		t.Fatal("expected exact resolve URL to be stored on config.URL")
	}
	if src.config.ServerURL != "" {
		t.Fatalf("expected legacy ServerURL to be empty for exact resolve URLs, got %q", src.config.ServerURL)
	}
}

func TestDiscoverRemoteSecretsSource_SkipsExactResolveURL(t *testing.T) {
	isolateDiscoveryEnv(t, false)
	t.Setenv("CONFIG_SERVER_URL", "https://config.example.test/api/configs/resolve?appName=shop%2Fserver&environment=prod&secretsMode=reveal")
	t.Setenv("CONFIG_SERVER_TOKEN", "token")

	if src := DiscoverRemoteSecretsSource(); src != nil {
		t.Fatalf("expected no separate secrets source for exact config resolve URL, got %#v", src)
	}
}

func TestDiscoverRemoteSource_PicksMetadataViaProbeFallback(t *testing.T) {
	// Bare GCE without K_SERVICE / GOOGLE_CLOUD_PROJECT: the TCP probe is
	// the only signal and must still trigger workload identity.
	isolateDiscoveryEnv(t, true) // env hints empty, probe forced positive
	t.Setenv("CONFIG_SERVER_URL", "https://config.putnami.test")

	src := DiscoverRemoteSource()
	if src == nil {
		t.Fatal("expected RemoteConfigSource, got nil")
	}
	if _, ok := src.config.TokenSource.(*GcpMetadataTokenSource); !ok {
		t.Errorf("expected GcpMetadataTokenSource via probe fallback, got %T", src.config.TokenSource)
	}
}

func TestDiscoverRemoteSource_FallsBackToEnvOffGCP(t *testing.T) {
	// Off-GCP, no operator override: discovery still wires EnvVarTokenSource
	// (which will error at request time) plus emits the startup warning.
	isolateDiscoveryEnv(t, false)
	t.Setenv("CONFIG_SERVER_URL", "https://config.putnami.test")

	src := DiscoverRemoteSource()
	if src == nil {
		t.Fatal("expected RemoteConfigSource, got nil")
	}
	if _, ok := src.config.TokenSource.(*EnvVarTokenSource); !ok {
		t.Errorf("expected EnvVarTokenSource fallback off-GCP, got %T", src.config.TokenSource)
	}
}

func TestDiscoverRemoteSource_EnvVarPrecedence(t *testing.T) {
	// Precedence: PUTNAMI_CLOUD_TOKEN > CONFIG_SERVER_TOKEN > PUTNAMI_TOKEN.
	// Most specific name wins so workspace setup remains predictable when
	// operators set more than one (intentionally during migration, or
	// unintentionally via leftover shell config).
	cases := []struct {
		name      string
		env       map[string]string
		wantValue string
	}{
		{
			name:      "PUTNAMI_CLOUD_TOKEN wins over CONFIG_SERVER_TOKEN",
			env:       map[string]string{"PUTNAMI_CLOUD_TOKEN": "cloud", "CONFIG_SERVER_TOKEN": "config"},
			wantValue: "cloud",
		},
		{
			name:      "CONFIG_SERVER_TOKEN wins over PUTNAMI_TOKEN",
			env:       map[string]string{"CONFIG_SERVER_TOKEN": "config", "PUTNAMI_TOKEN": "putnami"},
			wantValue: "config",
		},
		{
			name:      "PUTNAMI_TOKEN used when it's the only one set",
			env:       map[string]string{"PUTNAMI_TOKEN": "putnami"},
			wantValue: "putnami",
		},
		{
			name:      "PUTNAMI_CLOUD_TOKEN beats all three",
			env:       map[string]string{"PUTNAMI_CLOUD_TOKEN": "cloud", "CONFIG_SERVER_TOKEN": "config", "PUTNAMI_TOKEN": "putnami"},
			wantValue: "cloud",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateDiscoveryEnv(t, true) // probe positive too; env vars must still win
			t.Setenv("CONFIG_SERVER_URL", "https://config.putnami.test")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			src := DiscoverRemoteSource()
			if src == nil {
				t.Fatal("expected RemoteConfigSource, got nil")
			}
			if _, ok := src.config.TokenSource.(*EnvVarTokenSource); !ok {
				t.Fatalf("expected EnvVarTokenSource, got %T", src.config.TokenSource)
			}
			tok, err := src.config.TokenSource.Token(context.Background())
			if err != nil {
				t.Fatalf("token resolution failed: %v", err)
			}
			if tok != tc.wantValue {
				t.Errorf("expected token=%q, got %q", tc.wantValue, tok)
			}
		})
	}
}

// staticTokenSource is a test double returning a fixed token (or error).
type staticTokenSource struct {
	tok string
	err error
}

func (s staticTokenSource) Token(_ context.Context) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	return s.tok, nil
}

func TestRemoteConfigSource_TokenSource_Override(t *testing.T) {
	// CONFIG_SERVER_TOKEN must be ignored when TokenSource is set.
	t.Setenv("CONFIG_SERVER_TOKEN", "from-env")

	var gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"config":   map[string]any{},
			"resolved": true,
		})
	}))
	defer ts.Close()

	source := NewRemoteConfigSource(RemoteSourceConfig{
		ServerURL:   ts.URL,
		AppName:     "test-app",
		Environment: "production",
		// Token field intentionally unset to prove TokenSource wins.
		TokenSource: staticTokenSource{tok: "foo"},
	})
	if _, err := source.Load(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotAuth != "Bearer foo" {
		t.Errorf("expected Authorization=Bearer foo, got %q", gotAuth)
	}
}

func TestRemoteConfigSource_TokenSource_Default(t *testing.T) {
	// With TokenSource nil and Token set, the static token is used —
	// preserves the pre-TokenSource behavior for callers that haven't
	// migrated.
	var gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"config":   map[string]any{},
			"resolved": true,
		})
	}))
	defer ts.Close()

	source := NewRemoteConfigSource(RemoteSourceConfig{
		ServerURL:   ts.URL,
		AppName:     "test-app",
		Environment: "production",
		Token:       "bar",
	})
	if _, err := source.Load(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotAuth != "Bearer bar" {
		t.Errorf("expected Authorization=Bearer bar, got %q", gotAuth)
	}
}

func TestRemoteConfigSource_TokenSource_ErrorSkipsAuthHeader(t *testing.T) {
	// A TokenSource error should not crash the source — it should log
	// and send the request without Authorization, matching the unset
	// token behavior.
	var gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"config":   map[string]any{},
			"resolved": true,
		})
	}))
	defer ts.Close()

	source := NewRemoteConfigSource(RemoteSourceConfig{
		ServerURL:   ts.URL,
		AppName:     "test-app",
		Environment: "production",
		TokenSource: staticTokenSource{err: errors.New("metadata unreachable")},
	})
	if _, err := source.Load(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotAuth != "" {
		t.Errorf("expected no Authorization header on token error, got %q", gotAuth)
	}
}

func TestRemoteSecretsSource_TokenSource_Override(t *testing.T) {
	var gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"secrets":     map[string]any{},
			"resolved":    true,
			"schemaMatch": true,
		})
	}))
	defer ts.Close()

	source := NewRemoteSecretsSource(RemoteSecretsSourceConfig{
		ServerURL:   ts.URL,
		AppName:     "test-app",
		Environment: "production",
		Token:       "from-static-field",
		TokenSource: staticTokenSource{tok: "from-token-source"},
	})
	if _, err := source.Load(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotAuth != "Bearer from-token-source" {
		t.Errorf("expected TokenSource to win over Token, got %q", gotAuth)
	}
}

func TestRemoteConfigSource_SendsBearerToken(t *testing.T) {
	var gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"config":   map[string]any{},
			"resolved": true,
		})
	}))
	defer ts.Close()

	source := NewRemoteConfigSource(RemoteSourceConfig{
		ServerURL:   ts.URL,
		AppName:     "test-app",
		Environment: "production",
		Token:       "test-token",
	})
	if _, err := source.Load(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotAuth != "Bearer test-token" {
		t.Errorf("expected Authorization=Bearer test-token, got %q", gotAuth)
	}
}
