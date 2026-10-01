package jobs

import (
	"os"
	"path/filepath"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/releaseset"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// keyAsHost makes every key this package builds carry osClass, the OS class
// the host it names writes, for the rest of the test: store.OSClassWindows for
// a Windows host and "" for a POSIX one. It replaces a package variable, so
// the test that calls it does not run in parallel.
func keyAsHost(t *testing.T, osClass string) {
	t.Helper()
	identity := hostOSClass
	t.Cleanup(func() { hostOSClass = identity })
	hostOSClass = func(string) string { return osClass }
}

// D-W5 puts the OS class in the cache key only. A release-set member's
// selection fingerprint is compared with the one a publication recorded on
// another host, so a Windows host computes the fingerprint a Linux or macOS
// host recorded for the same tree, upstream keys included, while its execution
// keys stay apart from theirs.
func TestSelectionFingerprintIgnoresTheHostOSClass(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "library"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "library", "index.ts"), []byte("export const a = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	project := &workspace.Project{
		ID: "/library", Name: "@putnami/library", Path: "library", Version: "1.0.0", Type: "library",
		Metadata: releaseSetProjectMetadata(t, "npm", "@putnami/library"),
	}
	ws := workspace.NewWorkspace(root, &wsproto.Config{Name: "putnami"}, []*workspace.Project{project})
	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(root, ".putnami", "store")))

	build := cacheableJob("build", project.ID, project.Name, project.Path)
	build.Project = project
	packageJob := packageTaskFixture(project, "test-provider", "1.0.0", t.TempDir())
	packageJob.DependsOn = []string{build.Key()}
	planned := []*ScheduledJob{build, packageJob}

	memberKey := releaseset.MemberKey("npm", "@putnami/library")
	members := map[string]*workspace.Project{memberKey: project}

	fingerprint := func() string {
		t.Helper()
		got, err := SelectionFingerprints(ws, planned, nil, cache, testProfiles(), members)
		if err != nil {
			t.Fatalf("SelectionFingerprints: %v", err)
		}
		if got[memberKey] == "" {
			t.Fatalf("no fingerprint for %s in %v", memberKey, got)
		}
		return got[memberKey]
	}
	executionKeys := func() map[string]string {
		t.Helper()
		keys, err := PrecomputeKeys(ws, planned, nil, nil, cache, CacheBypass{})
		if err != nil {
			t.Fatalf("PrecomputeKeys: %v", err)
		}
		return keys
	}

	// The baseline is a POSIX host whatever host runs the test: on Windows the
	// keys carry the Windows OS class unless the test says otherwise.
	keyAsHost(t, "")
	posixFingerprint, posixKeys := fingerprint(), executionKeys()
	keyAsHost(t, store.OSClassWindows)
	if got := fingerprint(); got != posixFingerprint {
		t.Fatalf("the Windows OS class moved the selection fingerprint: %s != %s", got, posixFingerprint)
	}
	for jobKey, key := range executionKeys() {
		if key == posixKeys[jobKey] {
			t.Errorf("the execution key of %s ignores the Windows OS class", jobKey)
		}
	}
}
