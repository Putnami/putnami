package deliverycli

import (
	"io"
	"slices"
	"testing"

	"go.putnami.dev/cloud/extension/internal/deliverycli/internal/remotecache"
	cache "go.putnami.dev/protocol/cache"
	"go.putnami.dev/protocol/features/spectest"
)

type providerConfigurationCase struct {
	name       string
	configured string
	intent     cache.Mode
	offer      []string
	hosted     bool
	wantMode   cache.Mode
	wantEcho   bool
}

// providerConfigurationCases is the table both tests below read. The mode is
// core's intent, else the configured mode, else minimal for a hosted launch and
// full otherwise. The provider echoes
// provider-configuration and returns the mode only when core listed the
// capability.
func providerConfigurationCases() []providerConfigurationCase {
	offered := []string{cache.CapabilityProviderConfiguration}
	return []providerConfigurationCase{
		{name: "configured mode", configured: "toplevel", offer: offered, wantMode: cache.ModeToplevel, wantEcho: true},
		{name: "no configured mode", offer: offered, wantMode: cache.ModeFull, wantEcho: true},
		{name: "core intent wins", configured: "toplevel", intent: cache.ModeMinimal, offer: offered, wantMode: cache.ModeMinimal, wantEcho: true},
		{name: "hosted, no configured mode", offer: offered, hosted: true, wantMode: cache.ModeMinimal, wantEcho: true},
		{name: "hosted, configured mode", configured: "full", offer: offered, hosted: true, wantMode: cache.ModeFull, wantEcho: true},
		{name: "hosted, core intent wins", intent: cache.ModeToplevel, offer: offered, hosted: true, wantMode: cache.ModeToplevel, wantEcho: true},
		{name: "not offered", configured: "toplevel", wantMode: cache.ModeToplevel},
		{name: "not offered, no configured mode", wantMode: cache.ModeFull},
	}
}

// initializeWithConfiguration runs one initialize through the wire parse, which
// leaves an absent mode absent, and returns the session and the parsed result.
func initializeWithConfiguration(t *testing.T, tc providerConfigurationCase) (*providerSession, *cache.InitializeResult) {
	t.Helper()
	if tc.hosted {
		t.Setenv(SessionReporterIngestURLEnv, "https://ingest.invalid")
	} else {
		t.Setenv(SessionReporterIngestURLEnv, "")
	}
	root := t.TempDir()
	if tc.configured != "" {
		if err := WriteCacheConfig(root, &CacheConfig{Enabled: false, URL: "https://cache.invalid", Mode: tc.configured}); err != nil {
			t.Fatal(err)
		}
	}
	s := newProviderSession(root, io.Discard)
	t.Cleanup(s.close)
	payload, err := cache.MarshalPayload(&cache.InitializeParams{
		ProtocolVersion: cache.ProviderProtocolMinVersion,
		BlobExchangeDir: t.TempDir(),
		Mode:            tc.intent,
		Capabilities:    tc.offer,
	})
	if err != nil {
		t.Fatal(err)
	}
	resp := s.handle(&cache.ProviderRequest{ProtocolVersion: cache.ProviderProtocolMinVersion, ID: 1, Op: cache.OpInitialize, Payload: payload})
	if !resp.OK {
		t.Fatalf("initialize not OK: %+v", resp.Error)
	}
	res, diags := cache.ParseAndValidateInitializeResult(resp.Payload)
	if res == nil {
		t.Fatalf("invalid initialize result: %v", diags)
	}
	return s, res
}

// TestInitialize_SessionModePrecedence pins how the provider resolves the
// session mode: core's intent, else the configured mode, else minimal for a
// hosted launch and full otherwise.
func TestInitialize_SessionModePrecedence(t *testing.T) {
	spectest.Proves(t, "cloud/cache-provider-configuration", "provider-resolves-its-configuration",
		"the-session-mode-is-the-intent-else-the-configured-mode-else-minimal-when-hosted-else-full")
	t.Setenv(remotecache.URLEnv, "")
	t.Setenv(remotecache.ModeEnv, "")
	for _, tc := range providerConfigurationCases() {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := initializeWithConfiguration(t, tc)
			if s.mode != tc.wantMode {
				t.Errorf("session mode = %q, want %q", s.mode, tc.wantMode)
			}
		})
	}
}

// TestInitialize_ProviderConfigurationEcho pins what the provider tells core:
// it echoes provider-configuration and returns the resolved mode only when core
// listed the capability.
func TestInitialize_ProviderConfigurationEcho(t *testing.T) {
	spectest.Proves(t, "cache/remote-cache-contract", "the-provider-resolves-the-mode", "provider-configuration-echo-and-mode")
	t.Setenv(remotecache.URLEnv, "")
	t.Setenv(remotecache.ModeEnv, "")
	for _, tc := range providerConfigurationCases() {
		t.Run(tc.name, func(t *testing.T) {
			_, res := initializeWithConfiguration(t, tc)
			echoed := slices.Contains(res.Capabilities, cache.CapabilityProviderConfiguration)
			if echoed != tc.wantEcho {
				t.Errorf("provider-configuration echoed = %v, want %v (capabilities %v)", echoed, tc.wantEcho, res.Capabilities)
			}
			wantResultMode := cache.Mode("")
			if tc.wantEcho {
				wantResultMode = tc.wantMode
			}
			if res.Mode != wantResultMode {
				t.Errorf("result mode = %q, want %q", res.Mode, wantResultMode)
			}
		})
	}
}
