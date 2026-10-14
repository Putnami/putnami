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
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
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
	probe := cachekey.WorkspaceSourceState
	t.Cleanup(func() { cachekey.WorkspaceSourceState = probe })
	cachekey.WorkspaceSourceState = func(*store.CacheManager, string) string { return state }
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
	writeExecutable(t, filepath.Join(bin, "git"), flippingGitScript)
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
					if keys, err = cachekey.PrecomputeKeys(ws, planned, nil, nil, cache, cachekey.CacheBypass{}); err != nil {
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
				want, err := cachekey.PrecomputeKeys(ws, planned, nil, nil, newCache(), cachekey.CacheBypass{})
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
