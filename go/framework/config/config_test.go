package config

import (
	"context"
	stderrors "errors"
	"os"
	"sync"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

type ServerConfig struct {
	Host string `json:"host" default:"localhost"`
	Port int    `json:"port" default:"8080" env:"PUTNAMI_TEST_CONFIG_SERVER_PORT"`
}

type DatabaseConfig struct {
	URL         string `json:"url" env:"DATABASE_URL"`
	MaxConns    int    `json:"maxConns" default:"10"`
	AutoMigrate bool   `json:"autoMigrate" default:"false"`
}

type cancelAwareSource struct {
	loadCalls        int
	loadContextCalls int
}

func (s *cancelAwareSource) Name() string  { return "cancel-aware" }
func (s *cancelAwareSource) Priority() int { return 50 }
func (s *cancelAwareSource) Load() (map[string]any, error) {
	s.loadCalls++
	return map[string]any{"server": map[string]any{"port": 9000}}, nil
}
func (s *cancelAwareSource) LoadContext(ctx context.Context) (map[string]any, error) {
	s.loadContextCalls++
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestLoadDefaults(t *testing.T) {
	spectest.Proves(t, "go/typed-configuration", "precedence", "defaults-apply-without-a-source")
	def := Config[ServerConfig]("server")
	cfg, err := Load(def)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Host != "localhost" {
		t.Errorf("expected 'localhost', got %q", cfg.Host)
	}
	if cfg.Port != 8080 {
		t.Errorf("expected 8080, got %d", cfg.Port)
	}
}

func TestLoadFromMapSource(t *testing.T) {
	spectest.Proves(t, "go/typed-configuration", "precedence", "source-value-overrides-default")
	def := Config[ServerConfig]("server")
	source := NewMapSource("test", 50, map[string]any{
		"server": map[string]any{
			"host": "0.0.0.0",
			"port": 3000,
		},
	})

	cfg, err := Load(def, source)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Host != "0.0.0.0" {
		t.Errorf("expected '0.0.0.0', got %q", cfg.Host)
	}
	if cfg.Port != 3000 {
		t.Errorf("expected 3000, got %d", cfg.Port)
	}
}

func TestLoadContextPrefersContextSourceAndPropagatesCancellation(t *testing.T) {
	spectest.Proves(t, "go/typed-configuration", "cancellation", "load-context-prefers-context-source")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	source := &cancelAwareSource{}
	_, err := LoadContext(ctx, Config[ServerConfig]("server"), source)
	if !stderrors.Is(err, context.Canceled) {
		t.Fatalf("LoadContext error = %v, want context.Canceled", err)
	}
	if source.loadContextCalls != 1 {
		t.Fatalf("ContextSource.LoadContext calls = %d, want 1", source.loadContextCalls)
	}
	if source.loadCalls != 0 {
		t.Fatalf("Source.Load calls = %d, want 0 when ContextSource is available", source.loadCalls)
	}
}

func TestLoadContextFallsBackToSourceLoad(t *testing.T) {
	spectest.Proves(t, "go/typed-configuration", "cancellation", "load-context-falls-back-to-synchronous-source")
	source := NewMapSource("plain", 50, map[string]any{
		"server": map[string]any{"port": 9000},
	})

	cfg, err := LoadContext(context.Background(), Config[ServerConfig]("server"), source)
	if err != nil {
		t.Fatalf("LoadContext error = %v", err)
	}
	if cfg.Port != 9000 {
		t.Fatalf("Port = %d, want 9000 from plain Source.Load", cfg.Port)
	}
}

func TestLoadOptionalScalarFromMapSource(t *testing.T) {
	type Options struct {
		Enabled *bool `json:"enabled"`
	}
	def := Config[Options]("events.push")

	for _, tc := range []struct {
		name    string
		data    map[string]any
		wantNil bool
		want    bool
	}{
		{name: "unset", data: map[string]any{}, wantNil: true},
		{name: "true", data: map[string]any{"enabled": true}, want: true},
		{name: "false", data: map[string]any{"enabled": false}, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := NewMapSource("test", 50, map[string]any{
				"events": map[string]any{"push": tc.data},
			})
			cfg, err := Load(def, source)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantNil {
				if cfg.Enabled != nil {
					t.Fatalf("enabled = %v, want nil", *cfg.Enabled)
				}
				return
			}
			if cfg.Enabled == nil || *cfg.Enabled != tc.want {
				t.Fatalf("enabled = %v, want %v", cfg.Enabled, tc.want)
			}
		})
	}
}

func TestLoadOptionalScalarFromEnv(t *testing.T) {
	type Options struct {
		Enabled *bool `json:"enabled" env:"TEST_CONFIG_OPTIONAL_BOOL"`
	}
	t.Setenv("TEST_CONFIG_OPTIONAL_BOOL", "false")

	cfg, err := Load(Config[Options]("events.push"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Enabled == nil || *cfg.Enabled {
		t.Fatalf("enabled = %v, want pointer to false", cfg.Enabled)
	}
}

func TestLoadFromEnv(t *testing.T) {
	t.Setenv("PUTNAMI_TEST_CONFIG_SERVER_PORT", "9090")

	def := Config[ServerConfig]("server")
	cfg, err := Load(def)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Port != 9090 {
		t.Errorf("expected 9090 from env, got %d", cfg.Port)
	}
}

func TestLoadEnvOverridesSource(t *testing.T) {
	spectest.Proves(t, "go/typed-configuration", "precedence", "env-tag-overrides-sources")
	t.Setenv("PUTNAMI_TEST_CONFIG_SERVER_PORT", "4000")

	def := Config[ServerConfig]("server")
	source := NewMapSource("test", 50, map[string]any{
		"server": map[string]any{
			"port": 3000,
		},
	})

	cfg, err := Load(def, source)
	if err != nil {
		t.Fatal(err)
	}

	// Env should take precedence
	if cfg.Port != 4000 {
		t.Errorf("expected 4000 (env override), got %d", cfg.Port)
	}
}

func TestLoadDatabaseConfig(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost:5432/test")
	defer os.Unsetenv("DATABASE_URL")

	def := Config[DatabaseConfig]("database")
	cfg, err := Load(def)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.URL != "postgres://localhost:5432/test" {
		t.Errorf("expected postgres URL, got %q", cfg.URL)
	}
	if cfg.MaxConns != 10 {
		t.Errorf("expected 10, got %d", cfg.MaxConns)
	}
	if cfg.AutoMigrate != false {
		t.Error("expected autoMigrate=false")
	}
}

func TestLoadNestedPath(t *testing.T) {
	def := Config[ServerConfig]("services.api.server")
	source := NewMapSource("test", 50, map[string]any{
		"services": map[string]any{
			"api": map[string]any{
				"server": map[string]any{
					"host": "api.example.com",
					"port": 443,
				},
			},
		},
	})

	cfg, err := Load(def, source)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Host != "api.example.com" {
		t.Errorf("expected 'api.example.com', got %q", cfg.Host)
	}
	if cfg.Port != 443 {
		t.Errorf("expected 443, got %d", cfg.Port)
	}
}

func TestSourcePriority(t *testing.T) {
	spectest.Proves(t, "go/typed-configuration", "precedence", "sources-merge-low-to-high-priority")
	def := Config[ServerConfig]("server")
	low := NewMapSource("low", 10, map[string]any{
		"server": map[string]any{"host": "low-priority"},
	})
	high := NewMapSource("high", 90, map[string]any{
		"server": map[string]any{"host": "high-priority"},
	})

	cfg, err := Load(def, low, high)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Host != "high-priority" {
		t.Errorf("expected 'high-priority', got %q", cfg.Host)
	}
}

func TestLoadDoesNotMutateSource(t *testing.T) {
	def := Config[ServerConfig]("server")
	low := NewMapSource("low", 10, map[string]any{
		"server": map[string]any{"host": "low-host"},
	})
	high := NewMapSource("high", 90, map[string]any{
		"server": map[string]any{"host": "high-host", "port": 9999},
	})

	if _, err := Load(def, low, high); err != nil {
		t.Fatal(err)
	}

	// The low source must be untouched: merging must not write the high
	// source's host/port back into the low source's own nested map.
	lowServer := low.data["server"].(map[string]any)
	if got := lowServer["host"]; got != "low-host" {
		t.Errorf("low source host mutated: got %v, want low-host", got)
	}
	if _, leaked := lowServer["port"]; leaked {
		t.Errorf("high-only key leaked into low source map: %v", lowServer)
	}
}

func TestConcurrentLoadSharingSource(t *testing.T) {
	// Two concurrent Loads sharing one source must not race on the source's
	// retained nested maps. Run under -race to catch the regression.
	def := Config[ServerConfig]("server")
	shared := NewMapSource("shared", 10, map[string]any{
		"server": map[string]any{"host": "shared"},
	})

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			high := NewMapSource("high", 90, map[string]any{
				"server": map[string]any{"host": "high", "port": 8443},
			})
			if _, err := Load(def, shared, high); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
}

func TestConfigDIToken(t *testing.T) {
	def := Config[ServerConfig]("server")
	token := Token(def)

	if token.Key() == "" {
		t.Error("token key should not be empty")
	}
	if token.Name() == "" {
		t.Error("token name should not be empty")
	}
}

type MultiWordConfig struct {
	MaxConns int `json:"maxConns" env:"APP_MAX_CONNS"`
}

func TestEnvSourceLoad(t *testing.T) {
	os.Setenv("APP_HOST", "0.0.0.0")
	os.Setenv("APP_SERVER_PORT", "8443")
	defer os.Unsetenv("APP_HOST")
	defer os.Unsetenv("APP_SERVER_PORT")

	tree, err := NewEnvSource("APP").Load()
	if err != nil {
		t.Fatal(err)
	}
	if tree["host"] != "0.0.0.0" {
		t.Errorf("expected host=0.0.0.0, got %v", tree["host"])
	}
	// Each underscore introduces a nesting level: APP_SERVER_PORT -> server.port.
	server, ok := tree["server"].(map[string]any)
	if !ok {
		t.Fatalf("expected nested 'server' map, got %T", tree["server"])
	}
	if server["port"] != "8443" {
		t.Errorf("expected server.port=8443, got %v", server["port"])
	}
}

func TestEnvSourceMultiWordKeyIsNested(t *testing.T) {
	// EnvSource splits on every '_', so APP_MAX_CONNS becomes the nested path
	// max.conns and never reaches a camelCase JSON key like "maxConns".
	os.Setenv("APP_MAX_CONNS", "42")
	defer os.Unsetenv("APP_MAX_CONNS")

	tree, err := NewEnvSource("APP").Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, flat := tree["maxConns"]; flat {
		t.Error("did not expect EnvSource to produce a camelCase 'maxConns' key")
	}
	nested, ok := tree["max"].(map[string]any)
	if !ok || nested["conns"] != "42" {
		t.Errorf("expected EnvSource to nest as max.conns, got %v", tree)
	}
}

func TestLoadCamelCaseFieldViaEnvTag(t *testing.T) {
	// The documented workaround for a camelCase key unreachable through EnvSource:
	// bind it with a per-field env tag, which Load reads directly.
	os.Setenv("APP_MAX_CONNS", "42")
	defer os.Unsetenv("APP_MAX_CONNS")

	cfg, err := Load(Config[MultiWordConfig](""), NewEnvSource("APP"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxConns != 42 {
		t.Errorf("expected maxConns=42 via env tag, got %d", cfg.MaxConns)
	}
}

func TestDeepMerge(t *testing.T) {
	spectest.Proves(t, "go/typed-configuration", "precedence", "nested-maps-merge-recursively")
	dst := map[string]any{
		"a": map[string]any{
			"b": "original",
			"c": "keep",
		},
	}
	src := map[string]any{
		"a": map[string]any{
			"b": "overridden",
			"d": "new",
		},
	}

	deepMerge(dst, src)

	a := dst["a"].(map[string]any)
	if a["b"] != "overridden" {
		t.Errorf("expected 'overridden', got %v", a["b"])
	}
	if a["c"] != "keep" {
		t.Errorf("expected 'keep', got %v", a["c"])
	}
	if a["d"] != "new" {
		t.Errorf("expected 'new', got %v", a["d"])
	}
}
