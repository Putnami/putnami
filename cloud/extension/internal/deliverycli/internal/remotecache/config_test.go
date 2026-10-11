package remotecache

import (
	"context"
	"encoding/json"
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

// A session that holds a workspace machine token and no signed-in user cannot
// run the canonical recipe. The machine token then is the bearer, classed so a
// refusal names its true source and never re-mints it.
func TestConfig_MachineTokenReplacesTheCanonicalRecipe(t *testing.T) {
	t.Setenv(TokenEnv, "")
	metadataRan := false
	for _, test := range []struct {
		name   string
		source TokenSource
	}{
		{name: "canonical recipe", source: TokenSource{Command: cacheTokenRecipe()}},
		{name: "canonical recipe through the wrapper", source: TokenSource{Command: []string{"./putnamiw", "cloud", "token", "--for", "cache"}}},
		{name: "no source", source: TokenSource{metadataToken: func(context.Context, string) (string, error) {
			metadataRan = true
			return "metadata-id-token", nil
		}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := &Config{URL: "https://cache.putnami.cloud", Token: test.source}
			if !c.TakesMachineToken() {
				t.Fatal("TakesMachineToken = false; this source yields to a machine token")
			}
			if c.SendsMachineToken() {
				t.Fatal("SendsMachineToken = true before a token was handed over")
			}
			c.UseMachineToken("pkt_workspace_read")
			got, err := c.ResolveBearer(context.Background())
			if err != nil {
				t.Fatalf("ResolveBearer: %v", err)
			}
			if got.Token != "pkt_workspace_read" || got.Class != TokenClassCloudEnv || got.Class.Renewable() {
				t.Fatalf("bearer class = %q (renewable=%t), want the machine token classed cloud-env and non-renewable",
					got.Class, got.Class.Renewable())
			}
			if metadataRan {
				t.Fatal("the metadata source ran while a machine token is the bearer")
			}
		})
	}
}

// Only the exact canonical recipe yields. A near miss is a custom command, and
// a custom command keeps precedence. Nothing here is resolved, so no command
// runs.
func TestConfig_OnlyTheExactCanonicalRecipeYieldsToAMachineToken(t *testing.T) {
	t.Setenv(TokenEnv, "")
	for _, command := range [][]string{
		{"putnami", "cloud", "token", "--for", "cache", "--verbose"},
		{"putnami", "cloud", "token", "--for", "npm"},
		{"putnami", "cloud", "token"},
		{"putnami-cloud", "cloud", "token", "--for", "cache"},
		{"sh", "-c", "putnami cloud token --for cache"},
	} {
		c := &Config{URL: "https://cache.putnami.cloud", Token: TokenSource{Command: command}}
		c.UseMachineToken("pkt_workspace_read")
		if c.TakesMachineToken() || c.SendsMachineToken() {
			t.Errorf("command %q yielded to a machine token; only the canonical recipe does", strings.Join(command, " "))
		}
	}
}

// Every explicit source keeps precedence over a machine token: the cache token
// override, a custom command, a URL source, a persisted metadata audience and
// the metadata selector. The check runs when the bearer is resolved, so a
// machine token handed over earlier is never sent in their place.
func TestConfig_ExplicitSourcesKeepPrecedenceOverAMachineToken(t *testing.T) {
	const machine = "pkt_workspace_read"
	metadata := func(context.Context, string) (string, error) { return "metadata-id-token", nil }
	for _, test := range []struct {
		name     string
		cacheEnv string
		selector string
		source   TokenSource
		token    string
		class    TokenClass
	}{
		{name: "PUTNAMI_CACHE_TOKEN over the canonical recipe", cacheEnv: "pkt_cache_override",
			source: TokenSource{Command: cacheTokenRecipe()}, token: "pkt_cache_override", class: TokenClassEnv},
		{name: "PUTNAMI_CACHE_TOKEN with no source", cacheEnv: "pkt_cache_override",
			source: TokenSource{}, token: "pkt_cache_override", class: TokenClassEnv},
		{name: "custom command", source: TokenSource{Command: []string{"sh", "-c", "printf custom-command-token"}},
			token: "custom-command-token", class: TokenClassCommand},
		{name: "persisted metadata audience", source: TokenSource{Audience: "https://cache.example", metadataToken: metadata},
			token: "metadata-id-token", class: TokenClassMetadata},
		{name: "metadata selector over the canonical recipe", selector: "metadata",
			source: TokenSource{Command: cacheTokenRecipe(), metadataToken: metadata},
			token:  "metadata-id-token", class: TokenClassMetadata},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(TokenEnv, test.cacheEnv)
			t.Setenv(TokenSourceEnv, test.selector)
			t.Setenv(URLEnv, "")
			t.Setenv(ModeEnv, "")
			t.Setenv(ReadOnlyEnv, "")
			c := &Config{URL: "https://cache.putnami.cloud", Token: test.source}
			if err := c.ApplyEnv(); err != nil {
				t.Fatalf("ApplyEnv: %v", err)
			}
			if c.TakesMachineToken() {
				t.Fatal("TakesMachineToken = true; this source keeps precedence")
			}
			c.UseMachineToken(machine)
			if c.SendsMachineToken() {
				t.Fatal("SendsMachineToken = true; this source keeps precedence")
			}
			got, err := c.ResolveBearer(context.Background())
			if err != nil {
				t.Fatalf("ResolveBearer: %v", err)
			}
			if got.Token != test.token || got.Class != test.class {
				t.Fatalf("bearer class = %q, want %q from the configured source, not the machine token", got.Class, test.class)
			}
		})
	}
}

// A URL source keeps precedence too. It is checked without a request: the
// config must not yield, and the machine token must not be the bearer.
func TestConfig_URLSourceKeepsPrecedenceOverAMachineToken(t *testing.T) {
	t.Setenv(TokenEnv, "")
	t.Setenv(AllowInsecureCacheEnv, "")
	c := &Config{URL: "https://cache.putnami.cloud", Token: TokenSource{URL: "http://tokens.invalid/cache"}}
	c.UseMachineToken("pkt_workspace_read")
	if c.TakesMachineToken() || c.SendsMachineToken() {
		t.Fatal("a URL token source yielded to a machine token")
	}
	got, err := c.ResolveBearer(context.Background())
	if err == nil {
		t.Fatal("ResolveBearer answered without the URL source; plain http to a remote host must be refused")
	}
	if got.Token != "" || got.Class != TokenClassURL {
		t.Fatalf("bearer class = %q with a token of %d bytes, want an empty url-class bearer", got.Class, len(got.Token))
	}
}

// The machine token is execution-local: a config that holds one marshals
// without it, and a nil or empty config never sends one.
func TestConfig_MachineTokenIsNeverPersisted(t *testing.T) {
	t.Setenv(TokenEnv, "")
	c := &Config{URL: "https://cache.putnami.cloud", Token: TokenSource{Command: cacheTokenRecipe()}}
	c.UseMachineToken("pkt_workspace_read")
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), "pkt_workspace_read") {
		t.Fatal("the marshaled config carries the machine token")
	}

	var absent *Config
	if absent.TakesMachineToken() || absent.SendsMachineToken() {
		t.Fatal("a nil config takes a machine token")
	}
	empty := &Config{URL: "https://cache.putnami.cloud"}
	empty.UseMachineToken("")
	if empty.SendsMachineToken() {
		t.Fatal("an empty machine token is sent")
	}
}
