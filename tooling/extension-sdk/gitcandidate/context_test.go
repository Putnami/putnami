package gitcandidate

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	job "go.putnami.dev/protocol/job"
)

// candidateContext is the context of a job of the workspace at root whose
// task declared candidateFiles for the project app.
func candidateContext(root string) *job.Context {
	return &job.Context{WorkspaceRoot: root, Tree: &job.Tree{
		Directory:            "app",
		CandidateDirectories: []string{"app"},
		CandidateFiles: []job.CandidateFile{
			{Path: "app/deleted.go", Mode: job.CandidateModeRegular},
			{Path: "app/main.go", Mode: job.CandidateModeRegular},
			{Path: "app/run.sh", Mode: job.CandidateModeExecutable},
			{Path: "app/vendor/new.py", Mode: job.CandidateModeUntracked},
		},
	}}
}

// A job whose context carries the cut reads it without Git: with no git on
// PATH the tree lists the cut that exists on disk, relative to the root asked,
// leaves out a file the cut does not list, and fingerprints it from the modes
// the context gave.
func TestOpenFromAnswersFromTheContextWithoutGit(t *testing.T) {
	root := t.TempDir()
	write(t, root, "app/main.go", "package main\n")
	write(t, root, "app/run.sh", "#!/bin/sh\n")
	write(t, root, "app/vendor/new.py", "x = 1\n")
	write(t, root, "app/debug.log", "ignored\n")
	t.Setenv("PATH", t.TempDir())
	ctx := candidateContext(root)

	tree, err := OpenFrom(ctx, filepath.Join(root, "app"))
	if err != nil || tree == nil {
		t.Fatalf("OpenFrom(app) = %v, %v", tree, err)
	}
	if got, want := tree.Paths(), []string{"main.go", "run.sh", "vendor/new.py"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Paths() = %v, want %v", got, want)
	}
	if _, kind := tree.Resolve("debug.log"); kind != Missing {
		t.Errorf("debug.log resolves to %v, want Missing", kind)
	}
	if _, err := tree.Fingerprint(); err != nil {
		t.Errorf("Fingerprint() = %v", err)
	}
	vendor, err := OpenFrom(ctx, filepath.Join(root, "app", "vendor"))
	if err != nil || vendor == nil || !reflect.DeepEqual(vendor.Paths(), []string{"new.py"}) {
		t.Errorf("OpenFrom(app/vendor) = %v, %v", vendor, err)
	}
	// A root the cut does not cover asks Git, which is not on PATH.
	if outside, err := OpenFrom(ctx, root); err != nil || outside != nil {
		t.Errorf("OpenFrom(root) = %v, %v, want no tree", outside, err)
	}
}

// The workspace's cut reaches a job through a file of the run; a file whose
// digest does not match is not read, and the tree then comes from Git.
func TestOpenFromReadsTheWorkspaceListing(t *testing.T) {
	root := t.TempDir()
	write(t, root, "lib/lib.go", "package lib\n")
	t.Setenv("PATH", t.TempDir())
	data, digest, err := job.EncodeCandidateFiles([]job.CandidateFile{{Path: "lib/lib.go", Mode: job.CandidateModeRegular}})
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "candidates.json")
	if err := os.WriteFile(file, data, 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := &job.Context{WorkspaceRoot: root, Tree: &job.Tree{Directory: "app",
		WorkspaceCandidates: &job.CandidateListing{File: file, Digest: digest}}}
	tree, err := OpenFrom(ctx, root)
	if err != nil || tree == nil || !reflect.DeepEqual(tree.Paths(), []string{"lib/lib.go"}) {
		t.Fatalf("OpenFrom(root) = %v, %v", tree, err)
	}
	ctx.Tree.WorkspaceCandidates.Digest = digest[:63] + "0"
	if tree, err := OpenFrom(ctx, root); err != nil || tree != nil {
		t.Errorf("OpenFrom with a stale digest = %v, %v, want the Git answer, no tree", tree, err)
	}
}

// Without the member, OpenFrom is Open.
func TestOpenFromFallsBackToGit(t *testing.T) {
	root := repository(t, map[string]string{"a.md": "# a\n"})
	git(t, root, "add", "a.md")
	for _, ctx := range []*job.Context{nil, {WorkspaceRoot: root}, {WorkspaceRoot: root, Tree: &job.Tree{Directory: "."}}} {
		tree, err := OpenFrom(ctx, root)
		if err != nil || tree == nil || !reflect.DeepEqual(tree.Paths(), []string{"a.md"}) {
			t.Errorf("OpenFrom(%+v) = %v, %v", ctx, tree, err)
		}
	}
}

// A root the cut names as one entry, such as a nested repository, lists no
// contents in the cut: OpenFrom asks Git for it.
func TestOpenFromAsksGitForARootTheCutNamesAsOneEntry(t *testing.T) {
	root := t.TempDir()
	write(t, root, "app/nested/x.go", "package x\n")
	t.Setenv("PATH", t.TempDir())
	ctx := &job.Context{WorkspaceRoot: root, Tree: &job.Tree{Directory: "app", CandidateDirectories: []string{"app"},
		CandidateFiles: []job.CandidateFile{{Path: "app/nested", Mode: job.CandidateModeUntracked}}}}
	if _, ok := ContextCut(ctx, filepath.Join(root, "app", "nested")); ok {
		t.Error("the cut answered for a root it names as one entry")
	}
	if _, ok := ContextCut(ctx, filepath.Join(root, "app", "nested", "sub")); ok {
		t.Error("the cut answered below a root it names as one entry")
	}
	if _, ok := ContextCut(ctx, filepath.Join(root, "app")); !ok {
		t.Error("the cut did not answer for the project")
	}
}
