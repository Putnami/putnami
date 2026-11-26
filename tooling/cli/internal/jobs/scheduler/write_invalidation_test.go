package scheduler

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
)

// cutRepository makes the fixture's workspace the top of a Git work tree that
// ignores .putnami, so its keys list the workspace's `git:**` from the run's
// cut.
func cutRepository(t *testing.T, f *captureFixture) {
	t.Helper()
	writeFileAt(t, filepath.Join(f.ws.Root, ".gitignore"), ".putnami\n")
	writeFileAt(t, filepath.Join(f.ws.Root, captureTestProject, "src", "main.ts"), "export {}\n")
	treeKeyedGit(t, f.ws.Root, "init", "-q")
	treeKeyedGit(t, f.ws.Root, "add", "-A")
}

// workspaceCutCheck keys on the workspace's `git:**`, as a lint-docs task does.
func workspaceCutCheck() *ScheduledJob {
	job := declaredJob("lint~docs", "docs", &extension.TaskDeclaration{})
	job.JobDef.TaskCachePolicy = &extension.TaskCachePolicy{NoOutput: true,
		Key: &extension.TaskCacheKey{WorkspaceFiles: []string{"git:**"}}}
	return job
}

// gitListedKey is the key job has when its `git:**` is listed with Git.
func gitListedKey(t *testing.T, f *captureFixture, job *ScheduledJob) string {
	t.Helper()
	return keyOf(t, f, job, store.NewCacheManager(nil))
}

// A task's declared output, a restore and a source rewriter each make the next
// key of a job that keys the workspace's `git:**` the key Git's listing gives.
func TestAWriteOfTheRunMovesTheWorkspaceCutKey(t *testing.T) {
	spectest.Proves(t, treeFactsFeature, "candidate-cut-and-history-follow-the-key", "a-write-of-the-run-reads-the-cut-again")
	t.Run("a declared output", func(t *testing.T) {
		producer, check := layersProducer(), workspaceCutCheck()
		f := newCaptureFixture(t, producer, check)
		cutRepository(t, f)
		stale := keyOf(t, f, check, f.cache)
		writeFileAt(t, layerPath(f), "layer\n")
		if keyOf(t, f, check, f.cache) != stale {
			t.Fatal("the memo did not hold the key before the write, so this test cannot prove anything")
		}
		var mu sync.Mutex
		f.sched.finalizeExecutedJob(t.Context(), producer, &JobResult{Status: "success"}, false, "", "", nil, &mu, map[string]string{})
		if got, want := keyOf(t, f, check, f.cache), gitListedKey(t, f, check); got != want || got == stale {
			t.Fatalf("after the output, the key is %q, want Git's %q", got, want)
		}
	})
	t.Run("a restore", func(t *testing.T) {
		producer, check := layersProducer(), workspaceCutCheck()
		f := newCaptureFixture(t, producer, check)
		cutRepository(t, f)
		writeFileAt(t, layerPath(f), "layer\n")
		if !f.sched.storeDeclaredCapture(producer, capturedResult(nil), captureHashA) {
			t.Fatal("declared capture published no entry")
		}
		if err := os.RemoveAll(filepath.Dir(layerPath(f))); err != nil {
			t.Fatal(err)
		}
		stale := keyOf(t, f, check, f.cache)
		f.hitOrFail(t, producer, captureHashA)
		if got, want := keyOf(t, f, check, f.cache), gitListedKey(t, f, check); got != want || got == stale {
			t.Fatalf("after the restore, the key is %q, want Git's %q", got, want)
		}
	})
	t.Run("a source rewriter", func(t *testing.T) {
		sources := extension.ResourceRef{ID: extension.ResourceIDSources, Scope: extension.ResourceScopeProject}
		fixer := declaredJob("lint~fix", "fix", &extension.TaskDeclaration{MutatesSources: true}, sources)
		check := workspaceCutCheck()
		f := newCaptureFixture(t, fixer, check)
		cutRepository(t, f)
		stale := keyOf(t, f, check, f.cache)
		writeFileAt(t, filepath.Join(f.ws.Root, captureTestProject, "src", "added.ts"), "export {}\n")
		var mu sync.Mutex
		f.sched.finalizeExecutedJob(t.Context(), fixer, &JobResult{Status: "success"}, false, "", "", nil, &mu, map[string]string{})
		if got, want := keyOf(t, f, check, f.cache), gitListedKey(t, f, check); got != want || got == stale {
			t.Fatalf("after the rewriter, the key is %q, want Git's %q", got, want)
		}
	})
}

// A key another worker computes between the two steps of an invalidation
// leaves no digest of the cut before the write for a later key.
func TestAKeyBetweenTheInvalidationsKeepsNoStaleCut(t *testing.T) {
	spectest.Proves(t, treeFactsFeature, "candidate-cut-and-history-follow-the-key", "a-write-of-the-run-reads-the-cut-again")
	producer, check := layersProducer(), workspaceCutCheck()
	f := newCaptureFixture(t, producer, check)
	cutRepository(t, f)
	stale := keyOf(t, f, check, f.cache)
	writeFileAt(t, layerPath(f), "layer\n")
	saved := betweenInvalidations
	betweenInvalidations = func() { keyOf(t, f, check, f.cache) }
	t.Cleanup(func() { betweenInvalidations = saved })
	var mu sync.Mutex
	f.sched.finalizeExecutedJob(t.Context(), producer, &JobResult{Status: "success"}, false, "", "", nil, &mu, map[string]string{})
	if got, want := keyOf(t, f, check, f.cache), gitListedKey(t, f, check); got != want || got == stale {
		t.Fatalf("after a key between the invalidations, the key is %q, want Git's %q", got, want)
	}
}

// notesCheck keys on the workspace's text files among Git's candidates, a
// listing the run's cut does not answer, so each digest of it asks Git.
func notesCheck() *ScheduledJob {
	job := declaredJob("lint~notes", "notes", &extension.TaskDeclaration{})
	job.JobDef.TaskCachePolicy = &extension.TaskCachePolicy{NoOutput: true,
		Key: &extension.TaskCacheKey{WorkspaceFiles: []string{"git:**/*.txt"}}}
	return job
}

// A write under the workspace's .putnami keeps the memoized digest of Git
// candidates only when Git excludes the directory as a whole and lists nothing
// below it. Otherwise the next key lists the candidates again and holds what
// Git's listing gives, the written file included (tooling/cli ADR 0106).
func TestAWriteUnderAnExcludedPutnamiKeepsTheCandidateDigests(t *testing.T) {
	spectest.Proves(t, treeFactsFeature, "candidate-cut-and-history-follow-the-key", "a-write-of-the-run-reads-the-cut-again")
	for _, tc := range []struct {
		name, gitignore string
		// tracked is a file below .putnami the index tracks, or "".
		tracked string
		// linked makes .putnami/out a link to the workspace root, so the
		// write lands among the candidates.
		linked bool
		// putnamiLinked makes .putnami itself a link to a directory of the
		// work tree that Git does not ignore.
		putnamiLinked bool
		// unignored rewrites .gitignore without .putnami after a first write
		// under it, as a task of the run may.
		unignored bool
		// candidateLinked tracks gen/a.txt, then makes gen a link to a
		// directory under .putnami, where the write lands.
		candidateLinked bool
		keeps           bool
	}{
		{name: "ignored", gitignore: ".putnami\n", keeps: true},
		{name: "not ignored", gitignore: ""},
		{name: "ignored with a tracked file below", gitignore: ".putnami\n", tracked: "pinned.txt"},
		{name: "ignored, written through a link out of it", gitignore: ".putnami\n", linked: true},
		{name: "ignored, itself a link into the work tree", gitignore: ".putnami\n", putnamiLinked: true},
		{name: "no longer ignored after a write of the run", gitignore: ".putnami\n", unignored: true},
		{name: "ignored, a candidate read through a link into it", gitignore: ".putnami\n", candidateLinked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			check := notesCheck()
			f := newCaptureFixture(t, check)
			root := f.ws.Root
			writeFileAt(t, filepath.Join(root, ".gitignore"), tc.gitignore)
			writeFileAt(t, filepath.Join(root, "notes.txt"), "v1\n")
			treeKeyedGit(t, root, "init", "-q")
			treeKeyedGit(t, root, "add", "-A")
			if tc.tracked != "" {
				writeFileAt(t, filepath.Join(root, ".putnami", tc.tracked), "v1\n")
				treeKeyedGit(t, root, "add", "-f", filepath.Join(".putnami", tc.tracked))
			}
			if tc.linked {
				if err := os.RemoveAll(filepath.Join(root, ".putnami", "out")); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Join(root, ".putnami"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(root, filepath.Join(root, ".putnami", "out")); err != nil {
					t.Skipf("symbolic links: %v", err)
				}
			}
			linkedTarget := filepath.Join(root, ".putnami", "out", captureTestProject, "~build", "gen")
			if tc.candidateLinked {
				writeFileAt(t, filepath.Join(root, "gen", "a.txt"), "v1\n")
				treeKeyedGit(t, root, "add", "gen/a.txt")
				if err := os.RemoveAll(filepath.Join(root, "gen")); err != nil {
					t.Fatal(err)
				}
				writeFileAt(t, filepath.Join(linkedTarget, "a.txt"), "v1\n")
				if err := os.Symlink(linkedTarget, filepath.Join(root, "gen")); err != nil {
					t.Skipf("symbolic links: %v", err)
				}
			}
			if tc.putnamiLinked {
				if err := os.RemoveAll(filepath.Join(root, ".putnami")); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Join(root, "cache"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(root, "cache"), filepath.Join(root, ".putnami")); err != nil {
					t.Skipf("symbolic links: %v", err)
				}
			}
			if tc.unignored {
				// A first write under .putnami, which a key of the run
				// reaches, reads the cut while Git ignores it; then a task
				// stops ignoring it.
				keyOf(t, f, check, f.cache)
				first := filepath.Join(root, ".putnami", "out", captureTestProject, "~build", "first.txt")
				writeFileAt(t, first, "first\n")
				f.sched.invalidateWritten([]string{first})
				writeFileAt(t, filepath.Join(root, ".gitignore"), "")
				f.sched.invalidateWritten([]string{filepath.Join(root, ".gitignore")})
			}
			before := keyOf(t, f, check, f.cache)
			written := filepath.Join(root, ".putnami", "out", captureTestProject, "~lint", "report.txt")
			writeFileAt(t, written, "report\n")
			if tc.tracked != "" {
				written = filepath.Join(root, ".putnami", tc.tracked)
				writeFileAt(t, written, "v2\n")
			}
			if tc.candidateLinked {
				written = filepath.Join(linkedTarget, "a.txt")
				writeFileAt(t, written, "v2\n")
			}
			// An edit no invalidation names shows whether the memo kept the
			// digest from before the write.
			writeFileAt(t, filepath.Join(root, "notes.txt"), "v2\n")
			f.sched.invalidateWritten([]string{written})
			got := keyOf(t, f, check, f.cache)
			if tc.keeps {
				if got != before {
					t.Fatal("the write under an excluded .putnami dropped the digest of Git candidates")
				}
				return
			}
			if want := gitListedKey(t, f, check); got != want || got == before {
				t.Fatalf("after the write, the key is %q, want Git's %q", got, want)
			}
		})
	}
}

// workspaceTarCheck keys on every .tar file of the workspace, less what
// exclusions name, as a check that verifies archives across projects would.
func workspaceTarCheck(patterns ...string) *ScheduledJob {
	job := declaredJob("lint~archives", "archives", &extension.TaskDeclaration{})
	job.JobDef.TaskCachePolicy = &extension.TaskCachePolicy{NoOutput: true,
		Key: &extension.TaskCacheKey{WorkspaceFiles: append([]string{"**/*.tar"}, patterns...)}}
	return job
}

// A restore that writes a file a workspace pattern selects moves the key of a
// job that reads it, and the key is the one a fresh read of the tree gives. A
// restore its exclusions remove keeps the memoized digest, and the key is
// still the fresh one (tooling/cli ADR 0109).
func TestARestoreMovesAWorkspaceKeyOnlyWhenItsPatternsSelectTheRestoredFile(t *testing.T) {
	for _, tc := range []struct {
		name     string
		excludes []string
		moves    bool
	}{
		{name: "a restored file the patterns select", moves: true},
		{name: "a restored file the exclusions remove", excludes: []string{"!**/.gen/**"}, moves: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			producer, check := layersProducer(), workspaceTarCheck(tc.excludes...)
			f := newCaptureFixture(t, producer, check)
			writeFileAt(t, layerPath(f), "layer\n")
			if !f.sched.storeDeclaredCapture(producer, capturedResult(nil), captureHashA) {
				t.Fatal("declared capture published no entry")
			}
			if err := os.RemoveAll(filepath.Dir(layerPath(f))); err != nil {
				t.Fatal(err)
			}
			stale := keyOf(t, f, check, f.cache)
			f.hitOrFail(t, producer, captureHashA)
			got, fresh := keyOf(t, f, check, f.cache), keyOf(t, f, check, store.NewCacheManager(nil))
			if got != fresh {
				t.Fatalf("after the restore, the key is %q, a fresh read gives %q", got, fresh)
			}
			if moved := got != stale; moved != tc.moves {
				t.Fatalf("the restore moved the key = %v, want %v", moved, tc.moves)
			}
		})
	}
}
