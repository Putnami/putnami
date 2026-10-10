package remotecache

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// Real hosted execution selects metadata; each unit test owns its source.
func TestMain(m *testing.M) {
	_ = os.Unsetenv(TokenSourceEnv)
	os.Exit(m.Run())
}

func TestMetadataIdentityOverridesPersistedRecipeWithoutRewritingIt(t *testing.T) {
	t.Setenv(TokenSourceEnv, "metadata")
	t.Setenv(TokenEnv, "")
	t.Setenv(URLEnv, "https://execution-cache.example/")
	t.Setenv(ModeEnv, "")
	t.Setenv(ReadOnlyEnv, "true")
	for _, recipe := range []TokenSource{
		{Command: []string{"must-not-run-human-token-command"}},
		{URL: "https://must-not-request-token.example", Audience: "old-audience"},
		{},
	} {
		cfg := &Config{URL: "https://persisted-cache.example", Token: recipe}
		before, err := json.Marshal(cfg.Token)
		if err != nil {
			t.Fatal(err)
		}
		var audience string
		cfg.Token.metadataToken = func(_ context.Context, got string) (string, error) {
			audience = got
			return "metadata-credential", nil
		}
		if err := cfg.ApplyEnv(); err != nil {
			t.Fatal(err)
		}
		bearer, err := cfg.ResolveBearer(context.Background())
		if err != nil || bearer.Token != "metadata-credential" || bearer.Class != TokenClassMetadata {
			t.Fatalf("metadata resolution: class=%q err=%v", bearer.Class, err)
		}
		if audience != "https://execution-cache.example" || !cfg.ReadOnly {
			t.Fatalf("execution overrides: audience=%q readOnly=%t", audience, cfg.ReadOnly)
		}
		after, err := json.Marshal(cfg.Token)
		if err != nil {
			t.Fatal(err)
		}
		if string(before) != string(after) {
			t.Fatalf("persisted recipe changed: %s -> %s", before, after)
		}
		encoded, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), "metadata-credential") || strings.Contains(string(encoded), "metadataIdentity") {
			t.Fatal("execution identity must not be persisted")
		}
	}
}

func TestMetadataIdentityPreservesExplicitTokenAndDisabledCache(t *testing.T) {
	t.Setenv(TokenSourceEnv, "metadata")
	t.Setenv(TokenEnv, "explicit-credential")
	t.Setenv(URLEnv, "https://cache.example")
	t.Setenv(ModeEnv, "")
	t.Setenv(ReadOnlyEnv, "")
	disabled := false
	cfg := &Config{Enabled: &disabled, Token: TokenSource{
		Command: []string{"must-not-run"},
		metadataToken: func(context.Context, string) (string, error) {
			t.Fatal("explicit token must win over metadata")
			return "", nil
		},
	}}
	if err := cfg.ApplyEnv(); err != nil {
		t.Fatal(err)
	}
	if cfg.Active() {
		t.Fatal("identity selection must not re-enable a disabled cache")
	}
	bearer, err := cfg.ResolveBearer(context.Background())
	if err != nil || bearer.Token != "explicit-credential" || bearer.Class != TokenClassEnv || bearer.Class.Renewable() {
		t.Fatalf("explicit resolution: class=%q err=%v", bearer.Class, err)
	}
}

func TestMetadataIdentityRejectsUnknownSelection(t *testing.T) {
	t.Setenv(TokenSourceEnv, "invalid-sensitive-value")
	err := (&Config{}).ApplyEnv()
	if err == nil || !strings.Contains(err.Error(), TokenSourceEnv) {
		t.Fatalf("expected actionable identity selector error, got %v", err)
	}
	if strings.Contains(err.Error(), "invalid-sensitive-value") {
		t.Fatal("invalid identity selector must not be echoed")
	}
}

func TestUnsetIdentitySelectorPreservesConfiguredSource(t *testing.T) {
	t.Setenv(TokenSourceEnv, "")
	t.Setenv(TokenEnv, "")
	t.Setenv(URLEnv, "")
	t.Setenv(ModeEnv, "")
	t.Setenv(ReadOnlyEnv, "")
	cfg := &Config{Token: TokenSource{Command: []string{"existing-token-command"}}}
	if err := cfg.ApplyEnv(); err != nil {
		t.Fatal(err)
	}
	if cfg.metadataIdentity || cfg.Token.Class() != TokenClassCommand {
		t.Fatal("ordinary local execution must retain its configured recipe")
	}
}
