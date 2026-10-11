package deliverycli

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"go.putnami.dev/cloud/extension/internal/deliverycli/internal/remotecache"
	cache "go.putnami.dev/protocol/cache"
)

// The host may select metadata for its real cache. Unit tests explicitly
// choose their own source and must not inherit that host authentication policy.
// An agent session exports its workspace machine token the same way; a test
// that reads the cache with one sets it itself. A hosted runner exports the
// cache URL, mode and read-only overrides to its gate, and the cache status
// builds its config from them, so no test inherits those either.
func TestMain(m *testing.M) {
	for _, name := range []string{
		remotecache.TokenSourceEnv,
		remotecache.TokenEnv,
		remotecache.URLEnv,
		remotecache.ModeEnv,
		remotecache.ReadOnlyEnv,
		cloudTokenEnv,
	} {
		_ = os.Unsetenv(name)
	}
	os.Exit(m.Run())
}

func TestNativeCacheIdentityNeedsNoRunnerConfig(t *testing.T) {
	t.Setenv(remotecache.TokenSourceEnv, "metadata")
	t.Setenv(remotecache.URLEnv, "https://cache.example")
	t.Setenv(remotecache.ReadOnlyEnv, "true")
	root := t.TempDir()
	var logs bytes.Buffer
	s := newProviderSession(root, &logs)
	defer s.close()
	result := mustInit(t, s, &cache.InitializeParams{BlobExchangeDir: t.TempDir(), Mode: cache.ModeFull})
	if !result.Ready || !s.readOnly {
		t.Fatalf("native provider readiness=%t readOnly=%t", result.Ready, s.readOnly)
	}
	if _, err := os.Stat(CachePath(root)); !os.IsNotExist(err) {
		t.Fatalf("provider must not create a cache config, stat=%v", err)
	}
}

func TestNativeCacheIdentityInvalidSelectionDisablesProvider(t *testing.T) {
	t.Setenv(remotecache.TokenSourceEnv, "invalid-sensitive-value")
	t.Setenv(remotecache.URLEnv, "https://cache.example")
	root := t.TempDir()
	writeLinkedManifest(t, root)
	var logs bytes.Buffer
	s := newProviderSession(root, &logs)
	defer s.close()
	result := mustInit(t, s, &cache.InitializeParams{BlobExchangeDir: t.TempDir(), Mode: cache.ModeFull})
	if result.Ready || s.client != nil {
		t.Fatal("invalid selection must not fall back to linked-workspace authentication")
	}
	if !strings.Contains(logs.String(), remotecache.TokenSourceEnv) || strings.Contains(logs.String(), "invalid-sensitive-value") {
		t.Fatalf("expected actionable redacted diagnostic: %s", logs.String())
	}
}
