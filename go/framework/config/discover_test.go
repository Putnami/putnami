package config

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func resetSourceDiscoverers(t *testing.T) {
	t.Helper()
	sourceDiscoverers.Lock()
	original := append([]SourceDiscoverer(nil), sourceDiscoverers.items...)
	sourceDiscoverers.items = nil
	sourceDiscoverers.Unlock()
	t.Cleanup(func() {
		sourceDiscoverers.Lock()
		sourceDiscoverers.items = original
		sourceDiscoverers.Unlock()
	})
}

func TestDiscoverSources_PicksUpSecretsFile(t *testing.T) {
	dir := t.TempDir()
	confDir := filepath.Join(dir, "conf")
	if err := os.Mkdir(confDir, 0755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(confDir, ".env.test.yaml"),
		[]byte("database:\n  host: db.local\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(confDir, ".secrets.test.yaml"),
		[]byte("database:\n  password: shh\n"), 0644); err != nil {
		t.Fatal(err)
	}

	origDir, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origDir)

	t.Setenv("APP_ENV", "test")

	sources := DiscoverSources()

	var found Source
	for _, s := range sources {
		if s.Priority() == 35 {
			found = s
			break
		}
	}
	if found == nil {
		names := make([]string, 0, len(sources))
		for _, s := range sources {
			names = append(names, s.Name())
		}
		t.Fatalf("expected secrets file source at priority 35; got %v", names)
	}
	data, err := found.Load()
	if err != nil {
		t.Fatalf("secrets source Load() failed: %v", err)
	}
	db, _ := data["database"].(map[string]any)
	if db["password"] != "shh" {
		t.Errorf("expected password=shh in secrets source, got %v", db["password"])
	}
}

func TestDiscoverSources_NoSecretsFileWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	origDir, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origDir)

	t.Setenv("APP_ENV", "test")

	for _, s := range DiscoverSources() {
		if s.Priority() == 35 {
			t.Errorf("unexpected secrets source when no .secrets.test.yaml exists: %v", s)
		}
	}
}

// A core-only Go application must not turn cloud-shaped environment variables
// into cloud initialization. The cloud package opts in by registering a source;
// without that registrar, even a configured endpoint is inert and local/env
// configuration remains sufficient to boot.
func TestDiscoverSources_CloudEnvironmentWithoutRegistrarStaysLocal(t *testing.T) {
	resetSourceDiscoverers(t)
	dir := t.TempDir()
	t.Chdir(dir)

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	t.Cleanup(server.Close)

	t.Setenv("CONFIG_SERVER_URL", server.URL)
	t.Setenv("PUTNAMI_CLOUD_TOKEN", "must-not-be-read-by-core")
	t.Setenv("CONFIG_DATA", "server:\n  port: 8080\n")

	sources := DiscoverSources()
	if got := len(sources); got != 1 {
		names := make([]string, 0, len(sources))
		for _, source := range sources {
			names = append(names, source.Name())
		}
		t.Fatalf("sources = %v, want only CONFIG_DATA", names)
	}
	if sources[0].Name() != "CONFIG_DATA" {
		t.Fatalf("source = %q, want CONFIG_DATA", sources[0].Name())
	}
	if _, err := sources[0].Load(); err != nil {
		t.Fatalf("load CONFIG_DATA: %v", err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("core config contacted the cloud endpoint %d times without a registered cloud source", got)
	}
}

func TestDiscoverSources_UsesRegisteredDiscoverers(t *testing.T) {
	resetSourceDiscoverers(t)
	dir := t.TempDir()
	origDir, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origDir)

	RegisterSourceDiscoverer(func() Source { return nil })
	RegisterSourceDiscoverer(func() Source {
		return NewMapSource("registered", 50, map[string]any{
			"server": map[string]any{"host": "registered-host"},
		})
	})

	var found Source
	for _, s := range DiscoverSources() {
		if s.Name() == "registered" {
			found = s
			break
		}
	}
	if found == nil {
		t.Fatal("expected registered source to be discovered")
	}

	cfg, err := Load(Config[ServerConfig]("server"), found)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "registered-host" {
		t.Errorf("expected registered-host, got %q", cfg.Host)
	}
}

func TestRegisterSourceDiscoverer_IgnoresNil(t *testing.T) {
	resetSourceDiscoverers(t)

	RegisterSourceDiscoverer(nil)

	if got := len(registeredSourceDiscoverers()); got != 0 {
		t.Errorf("registered discoverers = %d, want 0", got)
	}
}

func TestRegisterSourceDiscoverer_MatchesCloudRegistrarShape(t *testing.T) {
	resetSourceDiscoverers(t)

	var register func(func() Source) = RegisterSourceDiscoverer
	register(func() Source { return nil })

	if got := len(registeredSourceDiscoverers()); got != 1 {
		t.Errorf("registered discoverers = %d, want 1", got)
	}
}
