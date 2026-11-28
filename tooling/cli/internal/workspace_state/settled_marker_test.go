package workspace_state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/protocol/cache"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/cacheprovider"
)

// A settled marker holds for its commit, workspace and provider while every
// file of the scope keeps the stat it had when the provider read it and every
// variable its value; anything else publishes again.
func TestASettledMarkerHoldsWhileItsScopeDoes(t *testing.T) {
	spectest.Proves(t, "cache/remote-cache-contract", "a-settled-marker-is-kept-under-its-scope", "a-scope-that-moved-publishes-again")
	dir := t.TempDir()
	config, absent := filepath.Join(dir, "cache.json"), filepath.Join(dir, "auth.json")
	if err := os.WriteFile(config, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	written := time.Now()
	t.Setenv("PUTNAMI_CACHE_URL", "https://cache.example")
	scope := &cache.MarkerScope{Key: "sha256:k", Files: []string{config, absent}, Env: []string{"HOME"}, EnvPrefixes: []string{"PUTNAMI_CACHE_"}}

	// A file a stat cannot yet answer for is not witnessed, nor a relative one.
	if _, ok := cacheprovider.WitnessScope(scope, written.Add(time.Second)); ok {
		t.Fatal("a scope file written within the margin was witnessed")
	}
	if _, ok := cacheprovider.WitnessScope(&cache.MarkerScope{Key: "k", Files: []string{"cache.json"}}, written.Add(3*time.Second)); ok {
		t.Fatal("a relative scope file was witnessed")
	}
	witness, ok := cacheprovider.WitnessScope(scope, written.Add(3*time.Second))
	if !ok {
		t.Fatal("the scope was not witnessed")
	}
	if strings.Contains(witness.EnvDigest, "cache.example") || witness.Salt == "" {
		t.Fatalf("the variables are not digested with a salt: %+v", witness)
	}
	marker, ok := NewSettledMarker("c1", "ws", "p", witness)
	if !ok || !marker.Holds("c1", "ws", "p") {
		t.Fatalf("the marker does not hold right after it was recorded: %+v", marker)
	}
	for name, holds := range map[string]bool{
		"another commit":    marker.Holds("c2", "ws", "p"),
		"another workspace": marker.Holds("c1", "other", "p"),
		"another provider":  marker.Holds("c1", "ws", "q"),
	} {
		if holds {
			t.Errorf("%s: the marker holds", name)
		}
	}

	t.Setenv("PUTNAMI_CACHE_URL", "https://other.example")
	if marker.Holds("c1", "ws", "p") {
		t.Fatal("a changed variable of a prefix kept the marker")
	}
	if _, ok := NewSettledMarker("c1", "ws", "p", witness); ok {
		t.Fatal("a scope that moved since the provider read it was recorded")
	}
	t.Setenv("PUTNAMI_CACHE_URL", "https://cache.example")
	t.Setenv("PUTNAMI_CACHE_MODE", "minimal")
	if marker.Holds("c1", "ws", "p") {
		t.Fatal("a new variable of a prefix kept the marker")
	}
	if err := os.Unsetenv("PUTNAMI_CACHE_MODE"); err != nil {
		t.Fatal(err)
	}
	if !marker.Holds("c1", "ws", "p") {
		t.Fatal("the restored environment does not hold")
	}
	if err := os.WriteFile(absent, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if marker.Holds("c1", "ws", "p") {
		t.Fatal("a file that appeared kept the marker")
	}
	if err := os.Remove(absent); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, []byte(`{"url":"x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if marker.Holds("c1", "ws", "p") {
		t.Fatal("a rewritten file kept the marker")
	}
	if _, ok := cacheprovider.WitnessScope(nil, time.Now()); ok {
		t.Fatal("no scope was witnessed")
	}
	if _, ok := NewSettledMarker("c1", "ws", "p", nil); ok {
		t.Fatal("a marker without a witness was recorded")
	}
}

// A successful build keeps the settled marker of its entry, and a new commit's
// build is not answered by it.
func TestASuccessfulBuildKeepsItsSettledMarker(t *testing.T) {
	store := NewSessionStore(t.TempDir())
	if err := store.RecordSettledMarker("main", []string{"build"}, nil, &SettledMarker{SHA: "c1"}); err == nil {
		t.Fatal("a marker was kept with no build recorded")
	}
	if err := store.RecordSuccessfulBuild("main", []string{"build"}, nil, "c1"); err != nil {
		t.Fatal(err)
	}
	marker := &SettledMarker{SHA: "c1", Workspace: "ws", Provider: "p", Scope: cacheprovider.ScopeWitness{ScopeKey: "k", Salt: "s"}}
	if err := store.RecordSettledMarker("main", []string{"build"}, nil, marker); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordSuccessfulBuild("main", []string{"build"}, nil, "c2"); err != nil {
		t.Fatal(err)
	}
	kept, err := store.SettledMarker("main", []string{"build"}, nil)
	if err != nil || kept == nil || kept.SHA != "c1" {
		t.Fatalf("kept marker %+v (%v)", kept, err)
	}
	if kept.Holds("c2", "ws", "p") {
		t.Fatal("the marker of c1 answers c2")
	}
	if none, err := store.SettledMarker("main", []string{"test"}, nil); err != nil || none != nil {
		t.Fatalf("another command's marker %+v (%v)", none, err)
	}
}
