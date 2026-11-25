package cachekey

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	protocoljob "go.putnami.dev/protocol/job"
	"go.putnami.dev/sdk/extension/gittest"
	"go.putnami.dev/tooling/cli/internal/store"
)

// cutDigests is the `git:**` digest of the workspace at root listed with Git,
// and listed from the run's read of the cut, with their errors.
func cutDigests(t *testing.T, root string) (viaGit, viaCut string, gitErr, cutErr error) {
	t.Helper()
	plain := newRunCache(t)
	viaGit, gitErr = plain.HashFiles(root, []string{"git:**"}, nil, store.ProjectConfigScope{})
	run := newRunCache(t)
	RunTreeFacts(run, root)
	viaCut, cutErr = run.HashFiles(root, []string{"git:**"}, nil, store.ProjectConfigScope{})
	return viaGit, viaCut, gitErr, cutErr
}

// A `git:**` digest of the workspace listed from the run's cut equals the one
// the Git listing gives, in every state of the tree, and refuses what the Git
// listing refuses.
func TestTheWorkspaceDigestFromTheCutIsTheGitDigest(t *testing.T) {
	spectest.Proves(t, treeFactsFeature, "candidate-cut-and-history-follow-the-key", "a-candidate-cut-is-keyed-and-carried")
	root := treeFactsRepo(t)
	writeFileAt(t, filepath.Join(root, ".gitignore"), "dist/\n*.log\n")
	treeKeyedGit(t, root, "add", ".gitignore")
	treeKeyedGit(t, root, "commit", "-q", "-m", "chore: ignore logs")
	same := func(state string) {
		t.Helper()
		viaGit, viaCut, gitErr, cutErr := cutDigests(t, root)
		if gitErr != nil || cutErr != nil || viaGit != viaCut {
			t.Fatalf("%s: Git %q (%v), cut %q (%v): want one digest", state, viaGit, gitErr, viaCut, cutErr)
		}
	}
	refused := func(state string) {
		t.Helper()
		_, _, gitErr, cutErr := cutDigests(t, root)
		if gitErr == nil || cutErr == nil {
			t.Fatalf("%s: Git error %v, cut error %v: want both refused", state, gitErr, cutErr)
		}
	}
	same("tracked")
	writeFileAt(t, filepath.Join(root, "app", "new.go"), "package main\n")
	same("untracked, not ignored")
	writeFileAt(t, filepath.Join(root, "app", "debug.log"), "x\n")
	writeFileAt(t, filepath.Join(root, "lib", "dist", "out.js"), "x\n")
	same("ignored by a file rule and by a directory rule")
	treeKeyedGit(t, root, "update-index", "--chmod=+x", "app/main.go")
	same("an index mode change")
	treeKeyedGit(t, root, "add", "app/new.go")
	writeFileAt(t, filepath.Join(root, "app", "new.go"), "package main // changed\n")
	same("staged, then modified")
	if err := os.Remove(filepath.Join(root, "lib", "lib.go")); err != nil {
		t.Fatal(err)
	}
	same("deleted in the work tree")
	treeKeyedGit(t, root, "mv", "other/other.go", "other/renamed.go")
	same("renamed")
	if err := os.Symlink("main.go", filepath.Join(root, "app", "link.go")); err != nil {
		t.Fatal(err)
	}
	same("a symbolic link")

	nested := filepath.Join(root, "app", "nested")
	writeFileAt(t, filepath.Join(nested, "x.go"), "package x\n")
	treeKeyedGit(t, nested, "init", "-q")
	refused("a nested repository")
	if err := os.RemoveAll(nested); err != nil {
		t.Fatal(err)
	}
	same("the nested repository removed")

	head := strings.TrimSpace(treeKeyedGit(t, root, "rev-parse", "HEAD"))
	treeKeyedGit(t, root, "update-index", "--add", "--cacheinfo", "160000,"+head+",sub")
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	refused("a submodule")
	treeKeyedGit(t, root, "update-index", "--force-remove", "sub")
	if err := os.RemoveAll(filepath.Join(root, "sub")); err != nil {
		t.Fatal(err)
	}
	same("the submodule removed")

	treeKeyedGit(t, root, "add", "-A")
	treeKeyedGit(t, root, "commit", "-q", "-m", "chore: the states")
	treeKeyedGit(t, root, "checkout", "-q", "-b", "left")
	writeFileAt(t, filepath.Join(root, "app", "main.go"), "package main // left\n")
	treeKeyedGit(t, root, "commit", "-q", "-am", "left")
	treeKeyedGit(t, root, "checkout", "-q", "main")
	writeFileAt(t, filepath.Join(root, "app", "main.go"), "package main // right\n")
	treeKeyedGit(t, root, "commit", "-q", "-am", "right")
	merge := treeKeyedGitStatus(root, "merge", "-q", "left")
	if merge == nil {
		t.Fatal("the merge did not conflict")
	}
	refused("an unmerged path")
	treeKeyedGit(t, root, "merge", "--abort")
	same("the merge aborted")

	if err := os.WriteFile(filepath.Join(root, ".git", "index"), []byte("not an index"), 0o644); err != nil {
		t.Fatal(err)
	}
	refused("a failed enumeration")
}

// The cut a `git:**` digest reads is the run's current read: one read serves
// every digest and fact until a write of the run, and the digest after a write
// sees it.
func TestTheWorkspaceDigestFollowsTheRunsRead(t *testing.T) {
	spectest.Proves(t, treeFactsFeature, "candidate-cut-and-history-follow-the-key", "a-write-of-the-run-reads-the-cut-again")
	root := treeFactsRepo(t)
	var reads atomic.Int32
	saved := candidateFiles
	candidateFiles = func(dir string) ([]protocoljob.CandidateFile, error) { reads.Add(1); return saved(dir) }
	t.Cleanup(func() { candidateFiles = saved })
	cache := newRunCache(t)
	facts := RunTreeFacts(cache, root)
	digest := func() string {
		t.Helper()
		hash, err := cache.HashFiles(root, []string{"git:**"}, nil, store.ProjectConfigScope{})
		if err != nil {
			t.Fatal(err)
		}
		return hash
	}
	before := digest()
	if again := digest(); again != before || reads.Load() != 1 {
		t.Fatalf("a second digest moved or read the cut again (%d reads)", reads.Load())
	}
	written := filepath.Join(root, "app", "gen", "doc.md")
	writeFileAt(t, written, "# generated\n")
	cache.InvalidateFileHashesUnder([]string{filepath.Dir(written)})
	facts.InvalidateCut([]string{filepath.Dir(written)})
	if after := digest(); after == before || reads.Load() != 2 {
		t.Fatalf("the digest after a write of the run kept %q, or read the cut %d times", after, reads.Load())
	}
	if viaGit, viaCut, gitErr, cutErr := cutDigests(t, root); gitErr != nil || cutErr != nil || viaGit != viaCut || viaCut != digest() {
		t.Fatalf("after the write: Git %q, cut %q, run %q", viaGit, viaCut, digest())
	}
}

// treeKeyedGitStatus runs git as treeKeyedGit does and returns its error,
// for a command expected to fail, such as a conflicting merge.
func treeKeyedGitStatus(dir string, args ...string) error {
	cmd := gittest.Command(dir, append([]string{
		"-c", "user.name=T", "-c", "user.email=t@t.com", "-c", "commit.gpgsign=false",
	}, args...)...)
	cmd.Env = append(gittest.Environ(os.Environ()), "GIT_COMMITTER_DATE=2026-01-02T03:04:05Z", "GIT_AUTHOR_DATE=2026-01-02T03:04:05Z")
	return cmd.Run()
}

// A workspace below the top of its repository keeps the Git listing: a
// `git:**` digest covers the repository, so an edit above the workspace moves
// it, and the cut, which covers the workspace, is not used.
func TestANestedWorkspaceKeepsTheGitListing(t *testing.T) {
	spectest.Proves(t, treeFactsFeature, "candidate-cut-and-history-follow-the-key", "a-candidate-cut-is-keyed-and-carried")
	repo := t.TempDir()
	treeKeyedGit(t, repo, "init", "-q")
	writeFileAt(t, filepath.Join(repo, "top.txt"), "top\n")
	writeFileAt(t, filepath.Join(repo, "sibling", "b.go"), "package b\n")
	root := filepath.Join(repo, "ws")
	writeFileAt(t, filepath.Join(root, "app", "main.go"), "package main\n")
	viaGit, viaRun, gitErr, runErr := cutDigests(t, root)
	if gitErr != nil || runErr != nil || viaGit != viaRun {
		t.Fatalf("Git %q (%v), run %q (%v): want one digest", viaGit, gitErr, viaRun, runErr)
	}
	writeFileAt(t, filepath.Join(repo, "top.txt"), "edited\n")
	if after, _, _, _ := cutDigests(t, root); after == viaGit {
		t.Fatal("an edit above the workspace kept the digest")
	}
	if _, viaRunAfter, _, _ := cutDigests(t, root); viaRunAfter == viaRun {
		t.Fatal("an edit above the workspace kept the run's digest")
	}
}

// An untracked file whose name reads as a stage entry stays untracked in the
// cut, and its bytes stay in the digest.
func TestAnUntrackedNameThatReadsAsAStageEntryStaysUntracked(t *testing.T) {
	spectest.Proves(t, treeFactsFeature, "candidate-cut-and-history-follow-the-key", "a-candidate-cut-is-keyed-and-carried")
	root := treeFactsRepo(t)
	hostile := "100644 0000000000000000000000000000000000000000 0\tzz.txt"
	writeFileAt(t, filepath.Join(root, hostile), "x\n")
	files, err := candidateFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, file := range files {
		if file.Path == hostile {
			found = file.Mode == protocoljob.CandidateModeUntracked
		}
		if file.Path == "zz.txt" {
			t.Fatal("the name was read as a stage entry for zz.txt")
		}
	}
	if !found {
		t.Fatalf("the cut lists no untracked %q: %v", hostile, files)
	}
	if viaGit, viaRun, gitErr, runErr := cutDigests(t, root); gitErr != nil || runErr != nil || viaGit != viaRun {
		t.Fatalf("Git %q (%v), run %q (%v): want one digest", viaGit, gitErr, viaRun, runErr)
	}
}

// A .putnami directory Git ignores only in part, here its cache, holds
// candidates elsewhere: a write under it reads the cut again. A .putnami Git
// ignores as a whole does not, and Git is asked once per run.
func TestAPutnamiDirectoryIgnoredInPartIsReadAgain(t *testing.T) {
	spectest.Proves(t, treeFactsFeature, "candidate-cut-and-history-follow-the-key", "a-write-of-the-run-reads-the-cut-again")
	for _, tc := range []struct {
		name, rule string
		rereads    bool
	}{
		{"ignored in part", ".putnami/cache/\n", true},
		{"ignored as a whole", ".putnami\n", false},
		{"ignored as a whole directory", "/.putnami/\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := treeFactsRepo(t)
			writeFileAt(t, filepath.Join(root, ".gitignore"), "dist/\n"+tc.rule)
			writeFileAt(t, filepath.Join(root, ".putnami", "cache", "x"), "x\n")
			var reads, asks atomic.Int32
			savedFiles, savedIgnores := candidateFiles, ignoresDirectory
			candidateFiles = func(dir string) ([]protocoljob.CandidateFile, error) { reads.Add(1); return savedFiles(dir) }
			ignoresDirectory = func(root, rel string) bool { asks.Add(1); return savedIgnores(root, rel) }
			t.Cleanup(func() { candidateFiles, ignoresDirectory = savedFiles, savedIgnores })
			facts := NewTreeFacts(root)
			facts.currentCut()
			for range 2 {
				writeFileAt(t, filepath.Join(root, ".putnami", "out", "app", "y"), "y\n")
				facts.InvalidateCut([]string{filepath.Join(root, ".putnami", "out", "app")})
				facts.currentCut()
			}
			if want := int32(1); tc.rereads {
				want = 3
				if reads.Load() != want {
					t.Fatalf("read the cut %d times, want %d", reads.Load(), want)
				}
			} else if reads.Load() != want {
				t.Fatalf("read the cut %d times, want %d", reads.Load(), want)
			}
			if asks.Load() != 1 {
				t.Fatalf("asked Git %d times whether .putnami is ignored, want once", asks.Load())
			}
		})
	}
}
