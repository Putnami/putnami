package treefacts

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNestedRepositoryFindsAGitEntryBetweenTheDirectoryAndTheRoot(t *testing.T) {
	root := t.TempDir()
	mkdir := func(rel string) string {
		t.Helper()
		dir := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	mkdir(".git")
	plain := mkdir("libs/plain/lib")
	clone := mkdir("libs/clone/lib")
	mkdir("libs/clone/.git")
	submodule := mkdir("libs/sub/lib")
	if err := os.WriteFile(filepath.Join(root, "libs", "sub", ".git"), []byte("gitdir: ../../.git/modules/sub\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	atRoot := mkdir("libs/own")
	mkdir("libs/own/.git")

	for _, tc := range []struct {
		name string
		dir  string
		want bool
	}{
		{"a project of the workspace's repository", plain, false},
		{"the workspace root", root, false},
		{"a project in a nested clone", clone, true},
		{"a project in a submodule", submodule, true},
		{"a project at a nested repository's root", atRoot, true},
		{"a directory outside the workspace", filepath.Dir(root), false},
	} {
		if got := NestedRepository(root, tc.dir); got != tc.want {
			t.Errorf("%s: NestedRepository = %v, want %v", tc.name, got, tc.want)
		}
	}
}
