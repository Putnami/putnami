package jobs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/releaseset"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// keyAsSourceState makes every key this package builds carry state, the source
// state a workspace root in that state writes, for the rest of the test:
// store.SourceStateUnmanaged for a root with no repository and "" for a root
// inside one. It replaces a package variable, so the test that calls it does
// not run in parallel.
func keyAsSourceState(t *testing.T, state string) {
	t.Helper()
	probe := workspaceSourceState
	t.Cleanup(func() { workspaceSourceState = probe })
	workspaceSourceState = func(*store.CacheManager, string) string { return state }
}

// A task run where Git does not manage the root is stamped with no source
// binding, and the stamp is not a declared input of the tasks that read it. So
// the execution key itself carries the source state: an entry stored in one
// state never serves the other, in either direction, and the state reaches
// every downstream key through the upstream fold. A selection fingerprint
// belongs to a publication, which needs a repository, and carries no state.
func TestExecutionKeysSeparateTheTwoSourceStates(t *testing.T) {
	spectest.Proves(t, "cli/workspace-without-git", "source-state-keys-the-cache",
		"execution-keys-separate-the-two-source-states")
	root := t.TempDir()
	// Git discovery stops above the root, so the unreplaced probe below sees a
	// root with no repository wherever the temporary directory lives.
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(root))
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
	newCache := func() *store.CacheManager {
		return store.NewCacheManager(store.NewLocalStore(filepath.Join(root, ".putnami", "store")))
	}

	build := cacheableJob("build", project.ID, project.Name, project.Path)
	build.Project = project
	packageJob := packageTaskFixture(project, "test-provider", "1.0.0", t.TempDir())
	packageJob.DependsOn = []string{build.Key()}
	planned := []*ScheduledJob{build, packageJob}

	memberKey := releaseset.MemberKey("npm", "@putnami/library")
	members := map[string]*workspace.Project{memberKey: project}

	fingerprint := func() string {
		t.Helper()
		got, err := SelectionFingerprints(ws, planned, nil, newCache(), testProfiles(), members)
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
		keys, err := PrecomputeKeys(ws, planned, nil, nil, newCache(), CacheBypass{})
		if err != nil {
			t.Fatalf("PrecomputeKeys: %v", err)
		}
		if len(keys) != len(planned) {
			t.Fatalf("keys = %v, want one per planned job", keys)
		}
		return keys
	}

	// The unreplaced probe asks Git about the real root, which has no
	// repository: its keys are the unmanaged ones.
	probedKeys := executionKeys()

	keyAsSourceState(t, "")
	managedFingerprint, managedKeys := fingerprint(), executionKeys()
	keyAsSourceState(t, store.SourceStateUnmanaged)
	unmanagedKeys := executionKeys()
	if got := fingerprint(); got != managedFingerprint {
		t.Fatalf("the source state moved the selection fingerprint: %s != %s", got, managedFingerprint)
	}
	for jobKey, key := range unmanagedKeys {
		if key == managedKeys[jobKey] {
			t.Errorf("the execution key of %s is the same in both source states", jobKey)
		}
		if key != probedKeys[jobKey] {
			t.Errorf("the execution key of %s at a root with no repository is not the unmanaged key", jobKey)
		}
	}
}

// flippingGitScript stands in for git. Its first `rev-parse --git-dir` answers
// as $FIRST says, "managed" or "unmanaged", and every later one answers the
// opposite; each is recorded in $PROBES. Every other command fails as outside
// a repository, so no source binding resolves.
const flippingGitScript = `#!/bin/sh
if [ "$1" = rev-parse ] && [ "$2" = --git-dir ]; then
  echo probe >> "$PROBES"
  n=0
  while IFS= read -r _; do n=$((n+1)); done < "$PROBES"
  answer=$FIRST
  if [ "$n" -gt 1 ]; then
    if [ "$FIRST" = managed ]; then answer=unmanaged; else answer=managed; fi
  fi
  if [ "$answer" = managed ]; then echo .git; exit 0; fi
fi
echo "fatal: not a git repository (or any of the parent directories): .git" >&2
exit 128
`

// The version stamp's marker and the execution keys of one run read one answer
// to whether Git manages the root, so an output stamped with no source claim is
// never stored under a key computed as inside a repository. The stand-in git
// changes its answer after the first question: two questions would split the
// stamp from the key, in whichever order the run asks.
func TestTheKeyAndTheStampReadOneAnswer(t *testing.T) {
	spectest.Proves(t, "cli/workspace-without-git", "source-state-keys-the-cache",
		"the-key-and-the-stamp-read-one-answer")
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in git is a POSIX shell script")
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(flippingGitScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+"/bin"+string(os.PathListSeparator)+"/usr/bin")

	for _, first := range []string{"unmanaged", "managed"} {
		for _, stampFirst := range []bool{true, false} {
			name := first + "/key first"
			if stampFirst {
				name = first + "/stamp first"
			}
			t.Run(name, func(t *testing.T) {
				probes := filepath.Join(t.TempDir(), "probes")
				t.Setenv("PROBES", probes)
				t.Setenv("FIRST", first)
				root := t.TempDir()
				if err := os.MkdirAll(filepath.Join(root, "library"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, "library", "index.ts"), []byte("export const a = 1\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				project := &workspace.Project{ID: "/library", Name: "library", Path: "library"}
				ws := workspace.NewWorkspace(root, &wsproto.Config{Name: "putnami"}, []*workspace.Project{project})
				build := cacheableJob("build", project.ID, project.Name, project.Path)
				build.Project = project
				planned := []*ScheduledJob{build}
				newCache := func() *store.CacheManager {
					return store.NewCacheManager(store.NewLocalStore(filepath.Join(root, ".putnami", "store")))
				}

				cache := newCache()
				s := newScheduler(ws, planned, nil, SchedulerConfig{}, nil, cache)
				var keys map[string]string
				precompute := func() {
					t.Helper()
					var err error
					if keys, err = PrecomputeKeys(ws, planned, nil, nil, cache, CacheBypass{}); err != nil {
						t.Fatalf("PrecomputeKeys: %v", err)
					}
				}
				if stampFirst {
					s.prepareRun()
					precompute()
				} else {
					precompute()
					s.prepareRun()
				}

				data, err := os.ReadFile(filepath.Join(root, "library", ".gen", "version.json"))
				if err != nil {
					t.Fatal(err)
				}
				var stamp VersionInfo
				if err := json.Unmarshal(data, &stamp); err != nil {
					t.Fatal(err)
				}
				if len(stamp.CapabilityPackages) != 1 {
					t.Fatalf("stamp packages = %+v, want the library alone", stamp.CapabilityPackages)
				}
				if got, want := stamp.CapabilityPackages[0].SourceBindingUnavailable, first == "unmanaged"; got != want {
					t.Fatalf("stamp marker = %v, want %v: the first answer", got, want)
				}
				if recorded, _ := os.ReadFile(probes); strings.Count(string(recorded), "probe") != 1 {
					t.Fatalf("git was asked %d times whether it manages the root, want once", strings.Count(string(recorded), "probe"))
				}

				state := ""
				if first == "unmanaged" {
					state = store.SourceStateUnmanaged
				}
				keyAsSourceState(t, state)
				want, err := PrecomputeKeys(ws, planned, nil, nil, newCache(), CacheBypass{})
				if err != nil {
					t.Fatalf("PrecomputeKeys: %v", err)
				}
				if keys[build.Key()] == "" || keys[build.Key()] != want[build.Key()] {
					t.Fatalf("execution key = %q, want %q: the key of the stamp's source state", keys[build.Key()], want[build.Key()])
				}
			})
		}
	}
}
