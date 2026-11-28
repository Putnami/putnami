package deliverycli

import (
	"path/filepath"
	"slices"
	"testing"

	"go.putnami.dev/cloud/extension/internal/deliverycli/internal/remotecache"
	"go.putnami.dev/protocol/features/spectest"
)

// A session names the scope of its marker answers only when its credential is
// read from the files and variables it names: the signed-in user's recipe or a
// token variable. A configured command, a token URL or an inactive cache names
// none, and the key moves with the endpoint.
func TestASessionNamesTheScopeOfItsMarkerAnswersOnlyWhenItCanWitnessIt(t *testing.T) {
	spectest.Proves(t, "cache/remote-cache-contract", "a-settled-marker-is-kept-under-its-scope", "the-cloud-provider-names-a-scope-it-can-witness")
	root := t.TempDir()
	home := t.TempDir()
	t.Setenv("PUTNAMI_HOME", home)
	t.Setenv(remotecache.TokenEnv, "")
	s := &providerSession{workspaceRoot: root}

	recipe := &remotecache.Config{URL: "https://cache.example", Token: remotecache.TokenSource{Command: cacheTokenCommand()}}
	scope := s.markerScope(recipe)
	if scope == nil || scope.Key == "" {
		t.Fatalf("the signed-in recipe named no scope: %+v", scope)
	}
	for _, want := range []string{CachePath(root), filepath.Join(home, "auth.json"), filepath.Join(root, "putnami.workspace.json")} {
		if !slices.Contains(scope.Files, want) {
			t.Errorf("the scope %v does not name %s", scope.Files, want)
		}
	}
	if !slices.Contains(scope.EnvPrefixes, "PUTNAMI_CACHE_") {
		t.Errorf("the scope does not name the cache variables: %v", scope.EnvPrefixes)
	}
	other := s.markerScope(&remotecache.Config{URL: "https://other.example", Token: remotecache.TokenSource{Command: cacheTokenCommand()}})
	if other == nil || other.Key == scope.Key {
		t.Fatal("another endpoint kept the scope key")
	}

	for name, cfg := range map[string]*remotecache.Config{
		"a configured command": {URL: "https://cache.example", Token: remotecache.TokenSource{Command: []string{"gcloud", "auth", "print-identity-token"}}},
		"a token URL":          {URL: "https://cache.example", Token: remotecache.TokenSource{URL: "https://token.example"}},
		"an inactive cache":    {Token: remotecache.TokenSource{Command: cacheTokenCommand()}},
	} {
		if got := s.markerScope(cfg); got != nil {
			t.Errorf("%s named a scope: %+v", name, got)
		}
	}

	if (&providerSession{workspaceRoot: root, hosted: true}).markerScope(recipe) != nil {
		t.Fatal("a hosted run, whose credential arrives in authenticate, named a scope")
	}

	t.Setenv(remotecache.TokenEnv, "static-token")
	if s.markerScope(&remotecache.Config{URL: "https://cache.example"}) == nil {
		t.Fatal("a token variable named no scope")
	}
}
