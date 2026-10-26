package deliverycli

import (
	"io"
	"slices"
	"testing"

	"go.putnami.dev/cloud/extension/internal/deliverycli/internal/remotecache"
	cache "go.putnami.dev/protocol/cache"
	"go.putnami.dev/protocol/features/spectest"
)

// TestInitialize_ProviderConfiguration pins how the provider resolves the
// session mode and tells core about it. The mode is core's intent, else the
// configured mode, else full. The provider echoes provider-configuration and
// returns the mode only when core listed the capability. Each initialize goes
// through the wire parse, which leaves an absent mode absent.
func TestInitialize_ProviderConfiguration(t *testing.T) {
	spectest.Proves(t, "cache/remote-cache-contract", "the-provider-resolves-the-mode", "provider-configuration-echo-and-mode")
	spectest.Proves(t, "cloud/cache-provider-configuration", "provider-resolves-its-configuration",
		"the-session-mode-is-the-intent-else-the-configured-mode-else-full")
	t.Setenv(remotecache.URLEnv, "")
	t.Setenv(remotecache.ModeEnv, "")

	offered := []string{cache.CapabilityProviderConfiguration}
	cases := []struct {
		name       string
		configured string
		intent     cache.Mode
		offer      []string
		wantMode   cache.Mode
		wantEcho   bool
	}{
		{name: "configured mode", configured: "toplevel", offer: offered, wantMode: cache.ModeToplevel, wantEcho: true},
		{name: "no configured mode", offer: offered, wantMode: cache.ModeFull, wantEcho: true},
		{name: "core intent wins", configured: "toplevel", intent: cache.ModeMinimal, offer: offered, wantMode: cache.ModeMinimal, wantEcho: true},
		{name: "not offered", configured: "toplevel", wantMode: cache.ModeToplevel},
		{name: "not offered, no configured mode", wantMode: cache.ModeFull},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if tc.configured != "" {
				if err := WriteCacheConfig(root, &CacheConfig{Enabled: false, URL: "https://cache.invalid", Mode: tc.configured}); err != nil {
					t.Fatal(err)
				}
			}
			s := newProviderSession(root, io.Discard)
			defer s.close()
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
			if s.mode != tc.wantMode {
				t.Errorf("session mode = %q, want %q", s.mode, tc.wantMode)
			}
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
