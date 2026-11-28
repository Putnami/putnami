package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.putnami.dev/protocol/cache"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/gitread"
	"go.putnami.dev/tooling/cli/internal/cacheprovider"
	"go.putnami.dev/tooling/cli/internal/workspace_state"
)

type fakeMarkerPublisher struct {
	witness   *cacheprovider.ScopeWitness
	provider  string
	published []string
}

func (f *fakeMarkerPublisher) PublishRunMarker(_ context.Context, _, _ string, _ []string, _, sha, _ string) *cacheprovider.ScopeWitness {
	f.published = append(f.published, sha)
	return f.witness
}

func (f *fakeMarkerPublisher) ProviderIdentity() string { return f.provider }

// A run marker the provider settled under a scope is not published again at
// the same commit while the scope holds; a moved scope file, another provider
// or a new commit publishes again, and a provider that names no scope
// publishes every time.
func TestASettledRunMarkerIsPublishedOnceUnderItsScope(t *testing.T) {
	spectest.Proves(t, "cache/remote-cache-contract", "a-settled-marker-is-kept-under-its-scope", "the-cli-publishes-a-settled-marker-once-under-its-scope")
	f := newExecuteFixture(t)
	initCLISelectionGitRepo(t, f.wsRoot)
	head, err := gitread.HeadSHA(f.wsRoot)
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), "cache.json")
	if err := os.WriteFile(config, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2100 * time.Millisecond)
	sessions := workspace_state.NewSessionStore(f.wsRoot)
	publisher := &fakeMarkerPublisher{provider: "p"}
	record := func() {
		recordSuccessfulBuild(sessions, f.ws, f.wsRoot, []string{"build"}, nil, false, publisher, "")
	}

	record()
	record()
	if len(publisher.published) != 2 {
		t.Fatalf("published %v, want every run while no scope is named", publisher.published)
	}
	witness, ok := cacheprovider.WitnessScope(&cache.MarkerScope{Key: "sha256:k", Files: []string{config}}, time.Now())
	if !ok {
		t.Fatal("the scope was not witnessed")
	}
	publisher.witness = witness
	record()
	record()
	if len(publisher.published) != 3 || publisher.published[2] != head {
		t.Fatalf("published %v, want the commit once under the scope", publisher.published)
	}
	if err := os.WriteFile(config, []byte(`{"url":"x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	record()
	if len(publisher.published) != 4 {
		t.Fatalf("published %v, want a moved scope file to publish again", publisher.published)
	}
	publisher.provider = "q"
	record()
	if len(publisher.published) != 5 {
		t.Fatalf("published %v, want another provider to publish again", publisher.published)
	}
	runCLISelectionGit(t, f.wsRoot, "commit", "-q", "--allow-empty", "-m", "next")
	record()
	if len(publisher.published) != 6 || publisher.published[5] == head {
		t.Fatalf("published %v, want the new commit", publisher.published)
	}
}
