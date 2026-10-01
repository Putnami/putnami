package jobs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCacheVerifyWorkspaceObserverDetectsAddedChangedAndRemovedPaths(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	write := func(name, contents string) {
		t.Helper()
		filename := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("changed.txt", "before")
	write("removed.txt", "before")
	before, err := snapshotVerificationTree(root)
	if err != nil {
		t.Fatal(err)
	}
	write("changed.txt", "after")
	write("added.txt", "after")
	if err := os.Remove(filepath.Join(root, "removed.txt")); err != nil {
		t.Fatal(err)
	}
	after, err := snapshotVerificationTree(root)
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"added.txt", "changed.txt", "removed.txt"}
	got := diffVerificationTreeSnapshots(before, after)
	if len(got) != len(want) {
		t.Fatalf("changed paths = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("changed paths = %v, want %v", got, want)
		}
	}
}
