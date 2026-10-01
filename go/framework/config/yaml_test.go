package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestYAMLFileSource(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	os.WriteFile(path, []byte("server:\n  host: yaml-host\n  port: 5555\n"), 0644)

	def := Config[ServerConfig]("server")
	src := NewYAMLFileSource(path, 50)

	cfg, err := Load(def, src)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "yaml-host" {
		t.Errorf("expected 'yaml-host', got %q", cfg.Host)
	}
	if cfg.Port != 5555 {
		t.Errorf("expected 5555, got %d", cfg.Port)
	}
}

func TestYAMLFileSourceMissing(t *testing.T) {
	src := NewYAMLFileSource("/nonexistent/config.yaml", 50)
	data, err := src.Load()
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if data != nil {
		t.Error("missing file should return nil data")
	}
}

func TestYAMLDataSource(t *testing.T) {
	raw := []byte("server:\n  host: inline-host\n  port: 7777\n")
	def := Config[ServerConfig]("server")
	src := NewYAMLDataSource("test", 50, raw)

	cfg, err := Load(def, src)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "inline-host" {
		t.Errorf("expected 'inline-host', got %q", cfg.Host)
	}
	if cfg.Port != 7777 {
		t.Errorf("expected 7777, got %d", cfg.Port)
	}
}

func TestYAMLDataSourceEmpty(t *testing.T) {
	src := NewYAMLDataSource("empty", 50, nil)
	data, err := src.Load()
	if err != nil {
		t.Fatalf("empty data should not error: %v", err)
	}
	if data != nil {
		t.Error("empty data should return nil")
	}
}

func TestConfigDataEnvVar(t *testing.T) {
	os.Setenv("CONFIG_DATA", "server:\n  host: container-host\n  port: 9999\n")
	defer os.Unsetenv("CONFIG_DATA")

	sources := DiscoverSources()

	var found bool
	for _, s := range sources {
		if s.Name() == "CONFIG_DATA" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("CONFIG_DATA source not discovered")
	}

	def := Config[ServerConfig]("server")
	cfg, err := Load(def, sources...)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "container-host" {
		t.Errorf("expected 'container-host', got %q", cfg.Host)
	}
	if cfg.Port != 9999 {
		t.Errorf("expected 9999, got %d", cfg.Port)
	}
}

func TestEnvironmentDetection(t *testing.T) {
	// Default
	os.Unsetenv("APP_ENV")
	if env := Environment(); env != "local" {
		t.Errorf("expected 'local', got %q", env)
	}

	// Custom
	os.Setenv("APP_ENV", "production")
	defer os.Unsetenv("APP_ENV")
	if env := Environment(); env != "production" {
		t.Errorf("expected 'production', got %q", env)
	}
}

func TestDiscoverSourcesWithFiles(t *testing.T) {
	dir := t.TempDir()
	confDir := filepath.Join(dir, "conf")
	os.Mkdir(confDir, 0755)

	// Create base and env-specific config files
	os.WriteFile(filepath.Join(confDir, ".env.yaml"), []byte("server:\n  host: base\n"), 0644)
	os.WriteFile(filepath.Join(confDir, ".env.test.yaml"), []byte("server:\n  host: test\n"), 0644)

	// Change to temp dir for discovery
	origDir, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(origDir)

	os.Setenv("APP_ENV", "test")
	defer os.Unsetenv("APP_ENV")

	sources := DiscoverSources()

	// Should find base + env-specific
	if len(sources) < 2 {
		t.Fatalf("expected at least 2 sources, got %d", len(sources))
	}

	def := Config[ServerConfig]("server")
	cfg, err := Load(def, sources...)
	if err != nil {
		t.Fatal(err)
	}

	// Environment-specific (priority 20) should override base (priority 10)
	if cfg.Host != "test" {
		t.Errorf("expected 'test' (env override), got %q", cfg.Host)
	}
}

func TestAutoDiscoveryOnEmptySources(t *testing.T) {
	dir := t.TempDir()
	confDir := filepath.Join(dir, "conf")
	os.Mkdir(confDir, 0755)

	os.WriteFile(filepath.Join(confDir, ".env.yaml"), []byte("server:\n  host: discovered\n  port: 1234\n"), 0644)

	origDir, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(origDir)

	os.Unsetenv("APP_ENV")
	os.Unsetenv("CONFIG_DATA")

	def := Config[ServerConfig]("server")
	cfg, err := Load(def)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Host != "discovered" {
		t.Errorf("expected 'discovered' from auto-discovery, got %q", cfg.Host)
	}
	if cfg.Port != 1234 {
		t.Errorf("expected 1234 from auto-discovery, got %d", cfg.Port)
	}
}

func TestEnvOverridesYAML(t *testing.T) {
	dir := t.TempDir()
	confDir := filepath.Join(dir, "conf")
	os.Mkdir(confDir, 0755)

	os.WriteFile(filepath.Join(confDir, ".env.yaml"), []byte("server:\n  host: yaml-value\n  port: 3000\n"), 0644)

	origDir, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(origDir)

	t.Setenv("PUTNAMI_TEST_CONFIG_SERVER_PORT", "4000")
	os.Unsetenv("CONFIG_DATA")

	def := Config[ServerConfig]("server")
	cfg, err := Load(def)
	if err != nil {
		t.Fatal(err)
	}

	// YAML value for host
	if cfg.Host != "yaml-value" {
		t.Errorf("expected 'yaml-value', got %q", cfg.Host)
	}
	// env tag should override YAML for port
	if cfg.Port != 4000 {
		t.Errorf("expected 4000 (env override), got %d", cfg.Port)
	}
}
