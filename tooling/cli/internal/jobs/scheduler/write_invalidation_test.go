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
