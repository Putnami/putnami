package cache

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// A marker scope arrives only with its capability echo, with a key and with
// absolute files, and survives the strict round trip.
func TestAMarkerScopeNeedsItsEchoAKeyAndAbsoluteFiles(t *testing.T) {
	spectest.Proves(t, "cache/remote-cache-contract", "a-settled-marker-is-kept-under-its-scope", "the-marker-scope-wire-is-pinned")
	file := filepath.Join(t.TempDir(), "cache.json")
	valid := func() *InitializeResult {
		return &InitializeResult{
			ProtocolVersion: ProviderProtocolVersion,
			ProviderVersion: "1.0.0",
			Capabilities:    []string{CapabilityMarkerScope},
			MarkerScope:     &MarkerScope{Key: "sha256:abc", Files: []string{file}, Env: []string{"PUTNAMI_CACHE_URL"}, EnvPrefixes: []string{"PUTNAMI_CACHE_"}},
			Ready:           true,
		}
	}
	if diags := ValidateInitializeResult(valid()); len(diags) != 0 {
		t.Fatalf("a valid scope: %v", diags)
	}
	data, err := json.Marshal(valid())
	if err != nil {
		t.Fatal(err)
	}
	parsed, diags := ParseAndValidateInitializeResult(data)
	if len(diags) != 0 || parsed.MarkerScope == nil || parsed.MarkerScope.Key != "sha256:abc" || len(parsed.MarkerScope.Files) != 1 {
		t.Fatalf("round trip %+v, %v", parsed, diags)
	}
	for name, mutate := range map[string]func(*InitializeResult){
		"no echo":       func(r *InitializeResult) { r.Capabilities = nil },
		"no key":        func(r *InitializeResult) { r.MarkerScope.Key = " " },
		"relative file": func(r *InitializeResult) { r.MarkerScope.Files = []string{"cache.json"} },
	} {
		r := valid()
		mutate(r)
		if diags := ValidateInitializeResult(r); len(diags) == 0 {
			t.Errorf("%s: the scope was accepted", name)
		}
	}
}
