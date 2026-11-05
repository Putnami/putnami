package gitread

import (
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/tooling/sdd/extension/internal/gittest"
)

// initGitRepo creates a repository on main, with a test identity and one
// commit of README.md, which the tree and source-binding tests read.

func initGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	cmds := [][]string{
		{"init"},
		{"checkout", "-b", "main"},
	}
	for _, args := range cmds {
		cmd := gittest.Command(dir, args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	// Create initial commit
	file := filepath.Join(dir, "README.md")
	if err := os.WriteFile(file, []byte("# Test"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := gittest.Command(dir, "add", ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	cmd = gittest.Command(dir, "commit", "-m", "initial")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}

	return dir
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := gittest.Command(dir, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}
