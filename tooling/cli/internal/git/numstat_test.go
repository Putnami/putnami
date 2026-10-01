package git

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestParseNumstat_ReadsCountsAndBinaryPaths(t *testing.T) {
	t.Parallel()
	got := parseNumstat("3\t1\tsrc/a.go\x00-\t-\tlogo.png\x000\t7\tdocs/old.md\x00garbage\x00")
	want := map[string]LineCount{
		"src/a.go":    {Added: 3, Deleted: 1},
		"logo.png":    {Binary: true},
		"docs/old.md": {Deleted: 7},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseNumstat = %#v, want %#v", got, want)
	}
}

func TestDiffLineCounts_CountsCommittedAndWorkingTreeChanges(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "kept.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "kept.txt")
	runGit(t, dir, "commit", "-m", "kept")
	base := runGit(t, dir, "rev-parse", "HEAD")
	runGit(t, dir, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(dir, "added.txt"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "added.txt")
	runGit(t, dir, "commit", "-m", "add")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Changed\nmore\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Staged, then restored to its base content in the working tree: git
	// counts no line for it, and it is not untracked either.
	if err := os.WriteFile(filepath.Join(dir, "kept.txt"), []byte("staged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "kept.txt")
	if err := os.WriteFile(filepath.Join(dir, "kept.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := DiffLineCounts(dir, base[:len(base)-1])
	if err != nil {
		t.Fatalf("DiffLineCounts: %v", err)
	}
	want := map[string]LineCount{
		"added.txt":     {Added: 2},
		"README.md":     {Added: 2, Deleted: 1},
		"untracked.txt": {Untracked: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DiffLineCounts = %#v, want %#v (an untracked file carries no numstat)", got, want)
	}
}

func TestOnEpicBranch(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)
	addOriginMainRef(t, dir)
	patterns := []string{"epic/*"}

	if OnEpicBranch(dir, patterns) {
		t.Fatal("main is not on an epic branch")
	}
	runGit(t, dir, "checkout", "-b", "epic/size")
	commitNewFile(t, dir, "epic.txt")
	if !OnEpicBranch(dir, patterns) {
		t.Error("the epic branch itself must count as declared")
	}
	if OnEpicBranch(dir, nil) {
		t.Error("no configured epic branches must never declare one")
	}
	runGit(t, dir, "checkout", "-b", "slice")
	commitNewFile(t, dir, "slice.txt")
	if !OnEpicBranch(dir, patterns) {
		t.Error("a branch forked from the epic must count as declared")
	}
	runGit(t, dir, "checkout", "main")
	runGit(t, dir, "checkout", "-b", "unrelated")
	commitNewFile(t, dir, "other.txt")
	if OnEpicBranch(dir, patterns) {
		t.Error("a branch forked from the trunk is not on an epic")
	}
}
