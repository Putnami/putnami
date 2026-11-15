package deliverycli

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/cloud/extension/internal/deliverycli/internal/remotecache"
	"go.putnami.dev/protocol/cache"
	"go.putnami.dev/protocol/features/spectest"
)

// readOnlyInit initializes a session with core's read-only capability listed
// or not, and returns the result and the session's log.
func readOnlyInit(t *testing.T, root string, coreLists bool) (*cache.InitializeResult, string, *providerSession) {
	t.Helper()
	var logw bytes.Buffer
	s := newProviderSession(root, &logw)
	t.Cleanup(s.close)
	params := &cache.InitializeParams{ProtocolVersion: cache.ProviderProtocolMinVersion, BlobExchangeDir: t.TempDir()}
	if coreLists {
		params.Capabilities = []string{cache.CapabilityReadOnly}
	}
	result := mustInit(t, s, params)
	return result, logw.String(), s
}

// assertWritesNothing drives an upload and a marker write and fails when the
// session accepted either.
func assertWritesNothing(t *testing.T, s *providerSession) {
	t.Helper()
	upload := s.upload(&cache.ProviderRequest{ProtocolVersion: cache.ProviderProtocolVersion, ID: 2, Op: cache.OpUpload},
		latchTestUpload(strings.Repeat("a", cache.KeyLength)))
	if result, _ := cache.ParseAndValidateUploadResult(upload.Payload); result == nil || result.Accepted {
		t.Fatalf("upload = %+v, want Accepted=false on a read-only session", upload)
	}
}

func TestAMachineTokenSessionIsDeclaredReadOnly(t *testing.T) {
	spectest.Proves(t, "cloud/agent-session", "a-read-only-cache-credential-is-declared", "a-machine-token-session-is-declared-read-only")
	spectest.Proves(t, "cache/remote-cache-contract", "a-read-only-session-is-declared", "the-cloud-provider-declares-read-only-only-when-listed")
	machineTokenSession(t, testMachineToken)
	srv, writes := writeRefusingCacheServer(t)
	t.Setenv(remotecache.URLEnv, srv.URL)

	result, _, s := readOnlyInit(t, t.TempDir(), true)
	if !slices.Contains(result.Capabilities, cache.CapabilityReadOnly) {
		t.Fatalf("capabilities = %v, want read-only echoed for a machine token", result.Capabilities)
	}
	assertWritesNothing(t, s)
	if got := writes.Load(); got != 0 {
		t.Fatalf("write requests = %d, want none", got)
	}

	// Without core's offer nothing is echoed, and the session still writes
	// nothing.
	result, _, s = readOnlyInit(t, t.TempDir(), false)
	if slices.Contains(result.Capabilities, cache.CapabilityReadOnly) {
		t.Fatalf("capabilities = %v, want no read-only echo when core did not list it", result.Capabilities)
	}
	assertWritesNothing(t, s)
}

func TestADeveloperSessionIsDeclaredReadOnlyOffAHostedRun(t *testing.T) {
	spectest.Proves(t, "cloud/agent-session", "a-read-only-cache-credential-is-declared", "a-developer-session-is-declared-read-only-off-a-hosted-run")
	spectest.Proves(t, "cache/remote-cache-contract", "a-read-only-session-is-declared", "the-cloud-provider-declares-read-only-only-when-listed")
	home := machineTokenSession(t, "")
	signIn(t, home)
	srv, writes := writeRefusingCacheServer(t)
	root := t.TempDir()
	command, _ := canonicalRecipe(t)
	writeCacheConfig(t, root, srv.URL, map[string]any{"command": command})

	result, log, s := readOnlyInit(t, root, true)
	if !slices.Contains(result.Capabilities, cache.CapabilityReadOnly) {
		t.Fatalf("capabilities = %v, want read-only echoed for the signed-in user's cache token", result.Capabilities)
	}
	assertWritesNothing(t, s)
	mustSummary(t, s)
	if got := writes.Load(); got != 0 {
		t.Fatalf("write requests = %d, want none: the session knew at initialize", got)
	}
	if strings.Contains(log, "read-only") {
		t.Fatalf("the session printed a read-only line core prints: %q", log)
	}

	// An older core that does not list the capability still gets one line.
	_, log, _ = readOnlyInit(t, root, false)
	if got := strings.Count(log, "the remote cache is read-only for this run: the signed-in user's cache token"); got != 1 {
		t.Fatalf("read-only lines = %d, want one for a core that does not list the capability: %q", got, log)
	}
}

func TestAHostedOrOtherSessionIsNotDeclaredReadOnly(t *testing.T) {
	spectest.Proves(t, "cloud/agent-session", "a-read-only-cache-credential-is-declared", "a-hosted-or-other-session-is-not-declared-read-only")
	t.Run("a static cache token", func(t *testing.T) {
		machineTokenSession(t, "")
		srv, _ := writeRefusingCacheServer(t)
		t.Setenv(remotecache.URLEnv, srv.URL)
		t.Setenv(remotecache.TokenEnv, "developer-token")
		result, _, _ := readOnlyInit(t, t.TempDir(), true)
		if slices.Contains(result.Capabilities, cache.CapabilityReadOnly) {
			t.Fatalf("capabilities = %v: a static token learns from a refused write", result.Capabilities)
		}
	})
	t.Run("the recipe on a hosted run", func(t *testing.T) {
		home := machineTokenSession(t, "")
		signIn(t, home)
		srv, _ := writeRefusingCacheServer(t)
		root := t.TempDir()
		command, _ := canonicalRecipe(t)
		writeCacheConfig(t, root, srv.URL, map[string]any{"command": command})
		t.Setenv(SessionReporterIngestURLEnv, "https://delivery.example/v1/delivery/records")
		result, _, _ := readOnlyInit(t, root, true)
		if slices.Contains(result.Capabilities, cache.CapabilityReadOnly) {
			t.Fatalf("capabilities = %v: a hosted run writes", result.Capabilities)
		}
	})
	t.Run("a metadata identity", func(t *testing.T) {
		home := machineTokenSession(t, "")
		signIn(t, home)
		srv, _ := writeRefusingCacheServer(t)
		root := t.TempDir()
		command, _ := canonicalRecipe(t)
		writeCacheConfig(t, root, srv.URL, map[string]any{"command": command})
		t.Setenv(remotecache.TokenSourceEnv, "metadata")
		result, _, _ := readOnlyInit(t, root, true)
		if slices.Contains(result.Capabilities, cache.CapabilityReadOnly) {
			t.Fatalf("capabilities = %v: a metadata identity is the CI runner's", result.Capabilities)
		}
	})
	t.Run("a run credential", func(t *testing.T) {
		// Core lists run-credential only on a hosted run, whose launcher
		// exports the ingest base: the session takes the run's token.
		home := machineTokenSession(t, "")
		signIn(t, home)
		srv, _ := writeRefusingCacheServer(t)
		root := t.TempDir()
		command, _ := canonicalRecipe(t)
		writeCacheConfig(t, root, srv.URL, map[string]any{"command": command})
		t.Setenv(SessionReporterIngestURLEnv, "https://delivery.example/v1/delivery/records")
		var logw bytes.Buffer
		s := newProviderSession(root, &logw)
		t.Cleanup(s.close)
		result := mustInit(t, s, &cache.InitializeParams{ProtocolVersion: cache.ProviderProtocolMinVersion, BlobExchangeDir: t.TempDir(),
			Capabilities: []string{cache.CapabilityReadOnly, cache.CapabilityRunCredential}})
		if slices.Contains(result.Capabilities, cache.CapabilityReadOnly) {
			t.Fatalf("capabilities = %v: a run credential writes", result.Capabilities)
		}
	})
	t.Run("an unconfigured cache", func(t *testing.T) {
		machineTokenSession(t, testMachineToken)
		result, _, _ := readOnlyInit(t, t.TempDir(), true)
		if result.Ready || slices.Contains(result.Capabilities, cache.CapabilityReadOnly) {
			t.Fatalf("result = %+v: a session that serves nothing declares nothing", result)
		}
	})
}

func TestTheProviderLeavesTheReadOnlyLineToCore(t *testing.T) {
	spectest.Proves(t, "cloud/agent-session", "a-read-only-cache-credential-is-declared", "the-provider-leaves-the-read-only-line-to-core")
	machineTokenSession(t, testMachineToken)
	srv, _ := writeRefusingCacheServer(t)
	t.Setenv(remotecache.URLEnv, srv.URL)

	_, log, _ := readOnlyInit(t, t.TempDir(), true)
	if strings.Contains(log, "read-only") {
		t.Fatalf("the session printed a read-only line core prints: %q", log)
	}
	_, log, _ = readOnlyInit(t, t.TempDir(), false)
	if got := strings.Count(log, "the remote cache is read-only for this run"); got != 1 {
		t.Fatalf("read-only lines = %d, want one for a core that does not list the capability: %q", got, log)
	}
}
