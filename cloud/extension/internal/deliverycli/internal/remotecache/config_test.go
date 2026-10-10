package remotecache

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	cache "go.putnami.dev/protocol/cache"
)

func TestLoadConfig_MissingFileIsInactive(t *testing.T) {
	c, err := LoadConfig(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("LoadConfig on missing file: %v", err)
	}
	if c.Active() {
		t.Error("a missing config must be inactive")
	}
}

func TestLoadConfig_ParsesAndActive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")
	body := `{"enabled":true,"url":"https://cache.example","mode":"full","token":{"command":["putnami-cloud","cache-token"]}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	c, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !c.Active() {
		t.Error("config with url and enabled should be active")
	}
	if c.URL != "https://cache.example" || c.Mode != cache.ModeFull {
		t.Errorf("parsed config = %+v", c)
	}
	if len(c.Token.Command) != 2 || c.Token.Command[0] != "putnami-cloud" {
		t.Errorf("token source not parsed: %+v", c.Token)
	}
}

func TestConfig_Active(t *testing.T) {
	enabled, disabled := true, false
	cases := []struct {
		name string
		cfg  Config
		want bool
	}{
		{"no url", Config{}, false},
		{"url, enabled implicit", Config{URL: "u"}, true},
		{"url, enabled true", Config{URL: "u", Enabled: &enabled}, true},
		{"url, enabled false", Config{URL: "u", Enabled: &disabled}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.Active(); got != tc.want {
				t.Errorf("Active() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestConfig_ApplyEnvOverrides(t *testing.T) {
	t.Setenv(URLEnv, "https://env.example")
	t.Setenv(ModeEnv, "minimal")
	t.Setenv(ReadOnlyEnv, "true")

	c := &Config{URL: "https://file.example", Mode: cache.ModeFull}
	if err := c.ApplyEnv(); err != nil {
		t.Fatalf("ApplyEnv: %v", err)
	}
	if c.URL != "https://env.example" {
		t.Errorf("URL = %q, want the env override", c.URL)
	}
	if c.Mode != cache.ModeMinimal {
		t.Errorf("Mode = %q, want the env override", c.Mode)
	}
	if !c.ReadOnly {
		t.Error("ReadOnly = false, want the env override")
	}
}

func TestConfig_ApplyEnvRejectsMalformedReadOnly(t *testing.T) {
	t.Setenv(ReadOnlyEnv, "probably")
	c := &Config{}
	if err := c.ApplyEnv(); err == nil || !strings.Contains(err.Error(), ReadOnlyEnv) {
		t.Fatalf("ApplyEnv error = %v, want an actionable %s error", err, ReadOnlyEnv)
	}
}

func TestConfig_ResolveTokenDefaultsMetadataAudienceToCacheURL(t *testing.T) {
	t.Setenv(TokenEnv, "")
	var gotAudience string
	c := &Config{
		URL: "https://cache.putnami.cloud/",
		Token: TokenSource{metadataToken: func(_ context.Context, audience string) (string, error) {
			gotAudience = audience
			return "metadata-token", nil
		}},
	}
	got, err := c.ResolveToken(context.Background())
	if err != nil || got != "metadata-token" {
		t.Fatalf("ResolveToken = %q, %v", got, err)
	}
	if gotAudience != "https://cache.putnami.cloud" {
		t.Fatalf("metadata audience = %q, want cache URL origin", gotAudience)
	}
}

// The production CI path is the tokenless runner config: no
// PUTNAMI_CACHE_TOKEN, no persisted recipe, audience defaulted to the cache URL.
// It must classify as metadata — the one renewable class that lets a long run's
// expired bearer be re-minted instead of stranding every store on a 401.
func TestConfig_ResolveBearerClassifiesTheRunnerMetadataPath(t *testing.T) {
	t.Setenv(TokenEnv, "")
	c := &Config{
		URL: "https://cache.putnami.cloud/",
		Token: TokenSource{metadataToken: func(context.Context, string) (string, error) {
			return "metadata-id-token", nil
		}},
	}
	got, err := c.ResolveBearer(context.Background())
	if err != nil {
		t.Fatalf("ResolveBearer: %v", err)
	}
	if got.Token != "metadata-id-token" {
		t.Fatalf("token = %q, want the minted metadata ID token", got.Token)
	}
	if got.Class != TokenClassMetadata || !got.Class.Renewable() {
		t.Fatalf("class = %q (renewable=%t), want a renewable metadata class", got.Class, got.Class.Renewable())
	}
}

// The same config with an injected bearer must flip to the static class, or the
// runner would "refresh" a machine token it cannot re-derive.
func TestConfig_ResolveBearerInjectedTokenIsNotRenewable(t *testing.T) {
	t.Setenv(TokenEnv, "pkt_injected")
	c := &Config{
		URL: "https://cache.putnami.cloud",
		Token: TokenSource{metadataToken: func(context.Context, string) (string, error) {
			t.Fatal("the metadata source must not run while PUTNAMI_CACHE_TOKEN is set")
			return "", nil
		}},
	}
	got, err := c.ResolveBearer(context.Background())
	if err != nil {
		t.Fatalf("ResolveBearer: %v", err)
	}
	if got.Token != "pkt_injected" || got.Class != TokenClassEnv || got.Class.Renewable() {
		t.Fatalf("bearer = %+v (renewable=%t), want the injected token classed env and non-renewable",
			got, got.Class.Renewable())
	}
}
