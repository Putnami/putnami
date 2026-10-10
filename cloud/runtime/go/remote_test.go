package runtime

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRemoteConfigSource(t *testing.T) {
	// Mock config server
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/configs/resolve" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"config": map[string]any{
				"server": map[string]any{
					"host": "0.0.0.0",
					"port": 9090,
				},
			},
			"resolved": true,
		})
	}))
	defer ts.Close()

	source := NewRemoteConfigSource(RemoteSourceConfig{
		ServerURL:   ts.URL,
		AppName:     "test-app",
		Environment: "production",
		Timeout:     2 * time.Second,
	})

	if source.Name() != "config-server" {
		t.Errorf("expected name=config-server, got %s", source.Name())
	}
	if source.Priority() != 50 {
		t.Errorf("expected priority=50, got %d", source.Priority())
	}

	data, err := source.Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if data == nil {
		t.Fatal("expected data, got nil")
	}

	server, ok := data["server"].(map[string]any)
	if !ok {
		t.Fatal("expected server config block")
	}
	if server["host"] != "0.0.0.0" {
		t.Errorf("expected host=0.0.0.0, got %v", server["host"])
	}
}

// TestRemoteConfigSourceSendsConfigVersion pins the pull side of the version pin: when a
// config version pin is set, the source sends BOTH version and pinned=true — in
// the query on the exact-URL GET path (the prod pull shape) and in the body on
// the POST path — so the server resolves the exclusive, fail-loud frozen layer.
func TestRemoteConfigSourceSendsConfigVersion(t *testing.T) {
	var gotVersionQ, gotPinnedQ, gotBodyVersion string
	var gotBodyPinned bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/configs/resolve" {
			http.NotFound(w, r)
			return
		}
		gotVersionQ = r.URL.Query().Get("version")
		gotPinnedQ = r.URL.Query().Get("pinned")
		if r.Method == http.MethodPost {
			var body struct {
				Version string `json:"version"`
				Pinned  bool   `json:"pinned"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			gotBodyVersion = body.Version
			gotBodyPinned = body.Pinned
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"config": map[string]any{"server": map[string]any{"host": "pinned"}}, "resolved": true,
		})
	}))
	defer ts.Close()

	t.Run("exact URL GET appends version and pinned query", func(t *testing.T) {
		gotVersionQ, gotPinnedQ = "", ""
		source := NewRemoteConfigSource(RemoteSourceConfig{
			URL:         ts.URL + "/api/configs/resolve?appName=a&environment=prod&secretsMode=reveal",
			AppName:     "a",
			Environment: "prod",
			Version:     "rel_pin1",
			Pinned:      true,
			Timeout:     2 * time.Second,
		})
		if _, err := source.Load(); err != nil {
			t.Fatalf("Load: %v", err)
		}
		if gotVersionQ != "rel_pin1" || gotPinnedQ != "true" {
			t.Errorf("GET query version=%q pinned=%q, want rel_pin1/true", gotVersionQ, gotPinnedQ)
		}
	})

	t.Run("POST body carries version and pinned", func(t *testing.T) {
		gotBodyVersion, gotBodyPinned = "", false
		source := NewRemoteConfigSource(RemoteSourceConfig{
			ServerURL:   ts.URL,
			AppName:     "a",
			Environment: "prod",
			Version:     "rel_pin2",
			Pinned:      true,
			Timeout:     2 * time.Second,
		})
		if _, err := source.Load(); err != nil {
			t.Fatalf("Load: %v", err)
		}
		if gotBodyVersion != "rel_pin2" || !gotBodyPinned {
			t.Errorf("POST body version=%q pinned=%v, want rel_pin2/true", gotBodyVersion, gotBodyPinned)
		}
	})

	t.Run("a bare version without Pinned is additive (no pinned sent)", func(t *testing.T) {
		gotBodyVersion, gotBodyPinned = "", false
		source := NewRemoteConfigSource(RemoteSourceConfig{
			ServerURL:   ts.URL,
			AppName:     "a",
			Environment: "prod",
			Version:     "1.2.3", // e.g. a stray APP_VERSION-style value
			Timeout:     2 * time.Second,
		})
		if _, err := source.Load(); err != nil {
			t.Fatalf("Load: %v", err)
		}
		if gotBodyPinned {
			t.Error("a bare version must NOT send pinned=true (keeps additive semantics)")
		}
	})
}

// TestDiscoverRemoteSourceUsesConfigVersionOnly pins that discovery reads the
// version pin from CONFIG_VERSION and NEVER from APP_VERSION, and marks it pinned:
// APP_VERSION carries the build semver at runtime, and a pinned resolve is
// exclusive + fail-loud, so sending the semver would fail every boot. An unset
// CONFIG_VERSION leaves the source unpinned (version="", Pinned=false), even when
// APP_VERSION is set. Secrets discovery is deliberately not touched here.
func TestDiscoverRemoteSourceUsesConfigVersionOnly(t *testing.T) {
	t.Setenv("CONFIG_SERVER_URL", "https://config.putnami.test")
	t.Setenv("APP_NAME", "a")
	t.Setenv("APP_VERSION", "1.2.3")

	t.Setenv("CONFIG_VERSION", "rel_pin")
	if src := DiscoverRemoteSource(); src == nil || src.config.Version != "rel_pin" || !src.config.Pinned {
		t.Fatalf("CONFIG_VERSION must be used and marked pinned: got %+v", src)
	}

	t.Setenv("CONFIG_VERSION", "")
	if src := DiscoverRemoteSource(); src == nil || src.config.Version != "" || src.config.Pinned {
		t.Fatalf("unset CONFIG_VERSION must leave the source unpinned (not APP_VERSION): got %+v", src)
	}
}

func TestRemoteConfigSourceExactURL(t *testing.T) {
	var gotMethod, gotPath, gotQuery string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		if r.URL.Path != "/api/configs/resolve" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"config": map[string]any{
				"google": map[string]any{
					"clientId":     "id",
					"clientSecret": "secret",
				},
			},
			"resolved": true,
		})
	}))
	defer ts.Close()

	exactURL := ts.URL + "/api/configs/resolve?appName=shop%2Fserver&environment=prod&secretsMode=reveal"
	source := NewRemoteConfigSource(RemoteSourceConfig{
		URL:         exactURL,
		AppName:     "shop/server",
		Environment: "prod",
		Timeout:     2 * time.Second,
	})

	data, err := source.Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method = %s, want GET", gotMethod)
	}
	if gotPath != "/api/configs/resolve" {
		t.Errorf("path = %s, want /api/configs/resolve", gotPath)
	}
	if gotQuery != "appName=shop%2Fserver&environment=prod&secretsMode=reveal" {
		t.Errorf("query = %s", gotQuery)
	}
	google, ok := data["google"].(map[string]any)
	if !ok {
		t.Fatal("expected google config block")
	}
	if google["clientId"] != "id" || google["clientSecret"] != "secret" {
		t.Errorf("google config = %#v", google)
	}
}

// TestRemoteConfigSourceServerCanonicalizesProductionAlias pins, from the Go
// client's side, that a client configured with environment=production
// against a canonical-`prod` server resolves successfully, because the
// server canonicalizes at its boundary (trim + lowercase + production →
// prod). The mock mirrors the server's resolve intake: it only serves the
// prod-dimension data when the canonicalized request environment is prod,
// and 403s genuinely different environments. No client-side normalization
// exists — the client sends `production` verbatim.
func TestRemoteConfigSourceServerCanonicalizesProductionAlias(t *testing.T) {
	canonicalEnv := func(env string) string {
		env = strings.ToLower(strings.TrimSpace(env))
		if env == "production" {
			return "prod"
		}
		return env
	}
	var sentEnv string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/configs/resolve" {
			http.NotFound(w, r)
			return
		}
		var body struct {
			AppName     string `json:"appName"`
			Environment string `json:"environment"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		sentEnv = body.Environment
		if canonicalEnv(body.Environment) != "prod" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"config":   map[string]any{"server": map[string]any{"host": "prod-host"}},
			"resolved": true,
		})
	}))
	defer ts.Close()

	source := NewRemoteConfigSource(RemoteSourceConfig{
		ServerURL:   ts.URL,
		AppName:     "test-app",
		Environment: "production",
		Timeout:     2 * time.Second,
	})
	data, err := source.Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sentEnv != "production" {
		t.Errorf("client sent environment %q, want verbatim production (server owns normalization)", sentEnv)
	}
	server, ok := data["server"].(map[string]any)
	if !ok || server["host"] != "prod-host" {
		t.Fatalf("data = %#v, want prod-dimension config", data)
	}

	// A genuinely different environment still fails — server-side
	// canonicalization is alias mapping, not authorization widening.
	staging := NewRemoteConfigSource(RemoteSourceConfig{
		ServerURL:   ts.URL,
		AppName:     "test-app",
		Environment: "staging",
		Timeout:     2 * time.Second,
	})
	if data, err := staging.Load(); err != nil || data != nil {
		t.Errorf("staging Load() = (%v, %v), want (nil, nil) on 403", data, err)
	}
}

func TestRemoteConfigSourceUnreachable(t *testing.T) {
	source := NewRemoteConfigSource(RemoteSourceConfig{
		ServerURL:   "http://127.0.0.1:1", // unreachable port
		AppName:     "test-app",
		Environment: "local",
		Timeout:     100 * time.Millisecond,
	})

	data, err := source.Load()
	if err != nil {
		t.Fatalf("expected no error on failure, got %v", err)
	}
	if data != nil {
		t.Errorf("expected nil data on failure, got %v", data)
	}
}

func TestRemoteConfigSourceUnreachableRequired(t *testing.T) {
	source := NewRemoteConfigSource(RemoteSourceConfig{
		URL:         "http://127.0.0.1:1/api/configs/resolve?appName=test-app&environment=prod&secretsMode=reveal",
		AppName:     "test-app",
		Environment: "prod",
		Timeout:     100 * time.Millisecond,
		RetryBudget: -1,
		Required:    true,
	})

	data, err := source.Load()
	if err == nil {
		t.Fatal("expected required remote source to return an error")
	}
	if data != nil {
		t.Errorf("expected nil data on failure, got %v", data)
	}
}

func TestRemoteConfigSourceRetriesStaleRawURLThroughAudienceAfterNotFound(t *testing.T) {
	var staleCalls, fallbackCalls int
	stale := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		staleCalls++
		http.NotFound(w, nil)
	}))
	defer stale.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackCalls++
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if got := r.URL.Query().Get("appName"); got != "surfaces/workloads/console" {
			t.Errorf("appName = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"config":      map[string]any{"database": map[string]any{"name": "marketing"}},
			"resolved":    true,
			"schemaMatch": true,
		})
	}))
	defer fallback.Close()

	source := NewRemoteConfigSource(RemoteSourceConfig{
		URL:              stale.URL + "/api/configs/resolve?appName=surfaces%2Fworkloads%2Fconsole&environment=prod&secretsMode=reveal",
		AudienceFallback: fallback.URL,
		AppName:          "surfaces/workloads/console",
		Environment:      "prod",
		Required:         true,
	})

	data, err := source.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if staleCalls != 1 || fallbackCalls != 1 {
		t.Fatalf("calls = stale:%d fallback:%d, want one each", staleCalls, fallbackCalls)
	}
	database, ok := data["database"].(map[string]any)
	if !ok || database["name"] != "marketing" {
		t.Fatalf("data = %#v", data)
	}
}

func TestRemoteConfigSourceRetriesRequiredUntilReady(t *testing.T) {
	calls := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			http.Error(w, "warming up", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"config":      map[string]any{"server": map[string]any{"port": 4100}},
			"resolved":    true,
			"schemaMatch": true,
		})
	}))
	defer ts.Close()

	source := NewRemoteConfigSource(RemoteSourceConfig{
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
	server, ok := data["server"].(map[string]any)
	if !ok {
		t.Fatalf("expected server config block, got %#v", data)
	}
	if server["port"] != float64(4100) {
		t.Errorf("expected server.port=4100, got %#v", server["port"])
	}
}

func TestRemoteConfigSourceNotResolved(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"config":   map[string]any{},
			"resolved": false,
		})
	}))
	defer ts.Close()

	source := NewRemoteConfigSource(RemoteSourceConfig{
		ServerURL:   ts.URL,
		AppName:     "unknown-app",
		Environment: "local",
	})

	data, err := source.Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if data != nil {
		t.Errorf("expected nil for unresolved config, got %v", data)
	}
}

func TestRemoteConfigSourceEmptySchemaMatch(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"config":      map[string]any{},
			"resolved":    false,
			"schemaMatch": true,
		})
	}))
	defer ts.Close()

	source := NewRemoteConfigSource(RemoteSourceConfig{
		ServerURL:   ts.URL,
		AppName:     "empty-app",
		Environment: "prod",
		Required:    true,
	})

	data, err := source.Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if data == nil {
		t.Fatal("expected empty config map, got nil")
	}
	if len(data) != 0 {
		t.Errorf("expected empty config map, got %v", data)
	}
}

func TestRemoteConfigSourceCaching(t *testing.T) {
	calls := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"config":   map[string]any{"key": "value"},
			"resolved": true,
		})
	}))
	defer ts.Close()

	source := NewRemoteConfigSource(RemoteSourceConfig{
		ServerURL:   ts.URL,
		AppName:     "test",
		Environment: "local",
	})

	// Call Load twice
	source.Load()
	source.Load()

	if calls != 1 {
		t.Errorf("expected 1 server call (cached), got %d", calls)
	}
}

func TestRemoteConfigSourceLogsServerWarnings(t *testing.T) {
	var logs bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(oldLogger)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"config":      map[string]any{"server": map[string]any{"port": 3000}},
			"resolved":    true,
			"schemaMatch": false,
			"warnings":    []string{"missing required field: database.url"},
		})
	}))
	defer ts.Close()

	source := NewRemoteConfigSource(RemoteSourceConfig{
		ServerURL:   ts.URL,
		AppName:     "test-app",
		Environment: "production",
	})

	data, err := source.Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if data == nil {
		t.Fatal("expected data, got nil")
	}

	output := logs.String()
	if !strings.Contains(output, "config-server schema mismatch") {
		t.Fatalf("expected schema mismatch log, got %q", output)
	}
	if !strings.Contains(output, "missing required field: database.url") {
		t.Fatalf("expected server warning log, got %q", output)
	}
}

func TestServerURLIsSecure(t *testing.T) {
	cases := []struct {
		url  string
		want bool
	}{
		{"https://config.example.com", true},
		{"https://config.example.com:8443/base", true},
		{"http://localhost:8080", true},
		{"http://127.0.0.1:9000", true},
		{"http://[::1]:9000", true},
		{"http://config.example.com", false},  // cleartext to a remote host
		{"http://10.0.0.5:8080", false},       // private IP is still cleartext on the wire
		{"http://config.internal/api", false}, // internal DNS, still cleartext
		{"ftp://config.example.com", false},   // non-http(s) scheme
		{"://bad url", false},                 // unparseable
	}
	for _, c := range cases {
		if got := serverURLIsSecure(c.url); got != c.want {
			t.Errorf("serverURLIsSecure(%q) = %v, want %v", c.url, got, c.want)
		}
	}
}

func TestWarnIfInsecureServerURL(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	warnIfInsecureServerURL("https://config.example.com", "config-server")
	warnIfInsecureServerURL("http://localhost:8080", "config-server")
	if buf.Len() != 0 {
		t.Fatalf("expected no warning for secure/loopback URLs, got %q", buf.String())
	}

	warnIfInsecureServerURL("http://config.example.com", "secrets-server")
	out := buf.String()
	if !strings.Contains(out, "not HTTPS") {
		t.Errorf("expected an insecure-URL warning, got %q", out)
	}
	// The URL must not be echoed into logs.
	if strings.Contains(out, "config.example.com") {
		t.Errorf("warning should not include the server URL, got %q", out)
	}
}
