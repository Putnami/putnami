package remotecache

import (
	"context"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// A provider's settled answer carries the scope it named; an answer it did not
// settle, or a provider that names no scope, carries none, so the next run
// publishes again.
func TestAPublishedMarkerCarriesTheScopeTheProviderNamed(t *testing.T) {
	spectest.Proves(t, "cache/remote-cache-contract", "a-settled-marker-is-kept-under-its-scope", "the-cli-carries-the-scope-of-a-settled-answer")
	for _, tc := range []struct {
		name  string
		env   map[string]string
		scope string
	}{
		{"published", map[string]string{"FAKE_MARKER_SCOPE": "sha256:k"}, "sha256:k"},
		{"read-only", map[string]string{"FAKE_MARKER_SCOPE": "sha256:k", "FAKE_READ_ONLY_ECHO": "1"}, "sha256:k"},
		{"kept, in effect", map[string]string{"FAKE_MARKER_SCOPE": "sha256:k", "FAKE_MARKER_IN_EFFECT": "cafef00d"}, "sha256:k"},
		{"another marker in effect", map[string]string{"FAKE_MARKER_SCOPE": "sha256:k", "FAKE_MARKER_IN_EFFECT": "0ther"}, ""},
		{"no marker in the answer", map[string]string{"FAKE_MARKER_SCOPE": "sha256:k", "FAKE_MARKER_IN_EFFECT": "none"}, ""},
		{"no scope", nil, ""},
		{"not ready", map[string]string{"FAKE_MARKER_SCOPE": "sha256:k", "FAKE_NOT_READY": "1"}, ""},
		{"initialize failed", map[string]string{"FAKE_MARKER_SCOPE": "sha256:k", "FAKE_INITIALIZE_FAIL": "1"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useInProcessFakeProvider(t)
			remote, _ := loadFakeProvider(t, t.TempDir(), tc.env)
			if remote.ProviderIdentity() == "" {
				t.Fatal("a provider-backed cache names no provider")
			}
			got := ""
			if witness := remote.PublishRunMarker(context.Background(), "ws-id", "main", []string{"build"}, "", "cafef00d", ""); witness != nil {
				got = witness.ScopeKey
			}
			if got != tc.scope {
				t.Fatalf("scope %q, want %q", got, tc.scope)
			}
		})
	}
	var none *RemoteCache
	if none.PublishRunMarker(context.Background(), "ws-id", "main", []string{"build"}, "", "cafef00d", "") != nil || none.ProviderIdentity() != "" {
		t.Fatal("a run with no remote cache named a scope or a provider")
	}
}
