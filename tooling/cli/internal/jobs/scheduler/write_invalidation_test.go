package scheduler

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
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

// A restore drops the memoized digests of the outputs it wrote, and only
// those: a second hit over a destination that already holds the output writes
// nothing and invalidates nothing (ADR 0119).
func TestRestoreInvalidatesOnlyTheOutputsItWrote(t *testing.T) {
	job := declaredJob("build~emit", "emit", &extension.TaskDeclaration{
		Outputs: map[string]extension.DeclaredOutput{
			"out":  dirDeclaration(extension.OutputRootProject, "dist", false),
			"same": fileDeclaration(extension.OutputRootProject, "same.txt", false),
		},
	})
	f := newCaptureFixture(t, job)
	dist := filepath.Join(f.ws.Root, captureTestProject, "dist")
	same := filepath.Join(f.ws.Root, captureTestProject, "same.txt")
	writeFileAt(t, filepath.Join(dist, "index.js"), "cold\n")
	writeFileAt(t, same, "same\n")
	if !f.sched.storeDeclaredCapture(job, capturedResult(nil), captureHashA) {
		t.Fatal("declared capture did not publish an entry")
	}

	var invalidations int
	saved := betweenInvalidations
	betweenInvalidations = func() { invalidations++ }
	t.Cleanup(func() { betweenInvalidations = saved })

	// The memo holds a digest of each output, taken before the hits.
	distDigest := func() string {
		t.Helper()
		digest, err := f.cache.HashFiles(dist, nil, nil, store.ProjectConfigScope{})
		if err != nil {
			t.Fatal(err)
		}
		return digest
	}
	cold := distDigest()

	// One output differs on disk: the restore writes it and drops its digests.
	writeFileAt(t, filepath.Join(dist, "index.js"), "stale\n")
	f.hitOrFail(t, job, captureHashA)
	if invalidations != 1 {
		t.Fatalf("a restore that wrote an output invalidated %d times, want 1", invalidations)
	}
	if got := distDigest(); got != cold {
		t.Fatalf("digest after the restore = %s, want the captured tree's %s", got, cold)
	}

	// Every output is already on disk, unchanged since this run's reads of the
	// tree began: nothing is written, nothing invalidated.
	f.cache.SetTreeReadsSince(time.Now().Add(time.Second))
	f.hitOrFail(t, job, captureHashA)
	if invalidations != 1 {
		t.Fatalf("a restore over held outputs invalidated %d more times, want none", invalidations-1)
	}
	if got := readFileAt(t, filepath.Join(dist, "index.js")); got != "cold\n" {
		t.Fatalf("held output = %q, want the captured bytes", got)
	}
}

// A restore that fails may have rewritten any of its outputs, so it drops the
// digests of every one, the outputs it never reached included (ADR 0119).
func TestFailedRestoreInvalidatesEveryRecordedOutput(t *testing.T) {
	job := declaredJob("build~emit", "emit", &extension.TaskDeclaration{
		Outputs: map[string]extension.DeclaredOutput{
			"out":  dirDeclaration(extension.OutputRootProject, "dist", false),
			"same": fileDeclaration(extension.OutputRootProject, "same.txt", false),
		},
	})
	f := newCaptureFixture(t, job)
	project := filepath.Join(f.ws.Root, captureTestProject)
	dist := filepath.Join(project, "dist")
	same := filepath.Join(project, "same.txt")
	writeFileAt(t, filepath.Join(dist, "index.js"), "cold\n")
	writeFileAt(t, same, "same\n")
	if !f.sched.storeDeclaredCapture(job, capturedResult(nil), captureHashA) {
		t.Fatal("declared capture did not publish an entry")
	}
	entry, err := f.cache.LookupTaskEntry(captureHashA)
	if err != nil || entry == nil {
		t.Fatalf("lookup task entry: %v %v", entry, err)
	}

	sameDigest := func() string {
		t.Helper()
		digest, err := f.cache.HashFiles(project, []string{"same.txt"}, nil, store.ProjectConfigScope{})
		if err != nil {
			t.Fatal(err)
		}
		return digest
	}
	before := sameDigest()
	// A write the memo was not told of: only an invalidation that names
	// same.txt makes the next digest read it.
	writeFileAt(t, same, "changed\n")

	// The restore of dist must write, and its payload is gone, so it fails.
	writeFileAt(t, filepath.Join(dist, "index.js"), "stale\n")
	if err := os.RemoveAll(filepath.Join(entry.FilesDir, "out")); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	if hit := f.sched.restoreDeclaredCacheHit(t.Context(), job, captureHashA, entry, &mu, map[string]string{}); hit != nil {
		t.Fatal("a restore whose payload is gone served a hit")
	}
	if got := sameDigest(); got == before {
		t.Fatal("a failed restore kept the digest of an output it never reached")
	}
}

// A destination that holds the output but changed after this run began to
// read the tree may hold bytes another session wrote after a digest this run
// memoized: the restore counts it as written and drops that digest (ADR 0119).
func TestRestoreOverARecentlyChangedHeldOutputInvalidatesIt(t *testing.T) {
	job := declaredJob("build~emit", "emit", &extension.TaskDeclaration{
		Outputs: map[string]extension.DeclaredOutput{
			"out": dirDeclaration(extension.OutputRootProject, "dist", false),
		},
	})
	f := newCaptureFixture(t, job)
	dist := filepath.Join(f.ws.Root, captureTestProject, "dist")
	writeFileAt(t, filepath.Join(dist, "index.js"), "produced\n")
	if !f.sched.storeDeclaredCapture(job, capturedResult(nil), captureHashA) {
		t.Fatal("declared capture did not publish an entry")
	}
	distDigest := func() string {
		t.Helper()
		digest, err := f.cache.HashFiles(dist, nil, nil, store.ProjectConfigScope{})
		if err != nil {
			t.Fatal(err)
		}
		return digest
	}
	produced := distDigest()

	// This run memoizes the tree as it stood before another session wrote it.
	writeFileAt(t, filepath.Join(dist, "index.js"), "older\n")
	f.cache.InvalidateFileHashesUnder([]string{dist})
	older := distDigest()
	if older == produced {
		t.Fatal("the two trees share a digest")
	}
	// The other session writes the entry's bytes; this run is not told.
	writeFileAt(t, filepath.Join(dist, "index.js"), "produced\n")

	f.hitOrFail(t, job, captureHashA)
	if got := distDigest(); got != produced {
		t.Fatalf("digest after a restore over a recently written held output = %s, want the restored tree's %s", got, produced)
	}
}

// A restore whose .gen was already on disk, unchanged since the run began to
// read the tree, writes no output, but its re-stamp can still rewrite the
// stamp: the digests of the stamp it replaced are dropped (ADR 0119).
func TestHeldRestoreThatReStampsDropsTheStampDigest(t *testing.T) {
	job := declaredJob("build~generate", "generate", &extension.TaskDeclaration{
		Outputs: map[string]extension.DeclaredOutput{"gen": dirDeclaration(extension.OutputRootProject, ".gen", false)},
	})
	f := newCaptureFixture(t, job)
	gen := filepath.Join(f.ws.Root, captureTestProject, ".gen")
	writeFileAt(t, filepath.Join(gen, "api.ts"), "export {}\n")
	// The producing run's stamp names another identity than this run's.
	if err := writeVersionStamp(filepath.Join(gen, "version.json"), VersionInfo{Name: "producer", Version: "9.9.9"}); err != nil {
		t.Fatal(err)
	}
	if !f.sched.storeDeclaredCapture(job, capturedResult(nil), captureHashA) {
		t.Fatal("declared capture did not publish an entry")
	}
	before := readFileAt(t, filepath.Join(gen, "version.json"))
	digest := func() string {
		t.Helper()
		d, err := f.cache.HashFiles(gen, nil, nil, store.ProjectConfigScope{})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	digest()
	f.cache.SetTreeReadsSince(time.Now().Add(time.Second))
	f.hitOrFail(t, job, captureHashA)
	if readFileAt(t, filepath.Join(gen, "version.json")) == before {
		t.Fatal("the restore did not re-stamp a stamp of another identity")
	}
	memoized := digest()
	f.cache.InvalidateFileHashesUnder([]string{gen})
	if fresh := digest(); memoized != fresh {
		t.Fatalf("after a held restore that re-stamped, the memo answers %s, the disk holds %s", memoized, fresh)
	}
}

// A completed restore keeps the project's memoized source binding when the
// output the binding covers was already on disk, and drops it when the
// restore wrote it (ADR 0119).
func TestRestoreThatWroteNothingKeepsTheSourceBinding(t *testing.T) {
	ws := makeExecutorTestWorkspace(t)
	runVersionsGit(t, ws.Root, "init")
	project := &workspace.Project{ID: "/app", Name: "app", Path: "app"}
	ws.Projects = []*workspace.Project{project}
	projectRoot := filepath.Join(ws.Root, project.Path)
	writeFileAt(t, filepath.Join(projectRoot, "main.go"), "package main\n")
	s := &Scheduler{ws: ws, capabilitySourceBindings: newCapabilitySourceBindingMemo()}
	calls := countSourceBindings(s)
	job := &ScheduledJob{Project: project}
	covered := &store.TaskEntry{Outputs: []store.TaskEntryOutput{{
		ID: "openapi", Kind: extension.OutputKindFile, Root: extension.OutputRootProject,
		Path: "schema/openapi.json", State: store.TaskOutputPresent,
	}}}
	binding := func() string {
		t.Helper()
		result, _ := s.capabilitySourceBindings.sourceBinding(ws.Root, projectRoot)
		if result.err != nil {
			t.Fatalf("compute the binding: %v", result.err)
		}
		return result.binding
	}

	memoized := binding()
	s.invalidateCapabilitySourceBindingsForWritten(job, covered, map[string]bool{})
	if got := binding(); got != memoized || calls[projectRoot] != 1 {
		t.Fatalf("after a restore that wrote nothing: binding %q (memoized %q), computed %d times, want the memoized binding", got, memoized, calls[projectRoot])
	}
	s.invalidateCapabilitySourceBindingsForWritten(job, covered, map[string]bool{"openapi": true})
	if binding(); calls[projectRoot] != 2 {
		t.Fatalf("after a restore that wrote the covered output: computed %d times, want 2", calls[projectRoot])
	}
}
