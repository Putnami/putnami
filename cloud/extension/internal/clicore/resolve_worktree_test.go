package clicore

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFindAppDirDoesNotSelectAssistantWorktreeCopies(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	seed := func(path string) string {
		t.Helper()
		dir := filepath.Join(root, path)
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "putnami.json"), []byte(`{"name":"images/ci-runner"}`), 0644); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	seed(".context/review-c04/images/ci-runner")
	seed(".claude/worktrees/agent/images/ci-runner")
	for _, name := range []string{"images/ci-runner", "ci-runner"} {
		if got, err := FindAppDir(root, name); err == nil {
			t.Fatalf("private worktree satisfied %q as %s", name, got)
		}
	}
	want := seed("images/ci-runner")
	// macOS resolves /var into /private/var before discovery.
	want, err := filepath.EvalSymlinks(want)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"images/ci-runner", "ci-runner"} {
		if got, err := FindAppDir(root, name); err != nil || got != want {
			t.Fatalf("%q selected %s (%v), want %s", name, got, err, want)
		}
	}
}

// TestFindAppDirPrefersTheWorkingProject covers a same-name copy that the walk
// reaches first (".copy" sorts before "apps"). A task runs from its project
// root, so that project wins, and the walk decides only elsewhere.
func TestFindAppDirPrefersTheWorkingProject(t *testing.T) {
	root := t.TempDir()
	seed := func(path, name string) string {
		t.Helper()
		dir := filepath.Join(root, path)
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "putnami.json"), []byte(`{"name":"`+name+`"}`), 0644); err != nil {
			t.Fatal(err)
		}
		resolved, err := filepath.EvalSymlinks(dir)
		if err != nil {
			t.Fatal(err)
		}
		return resolved
	}
	copyDir := seed(".copy/apps/api", "apps/api")
	project := seed("apps/api", "apps/api")
	other := seed("apps/other", "apps/other")

	if got, err := FindAppDir(root, "apps/api"); err != nil || got != copyDir {
		t.Fatalf("outside the project the walk decides: got %s (%v), want %s", got, err, copyDir)
	}
	t.Chdir(project)
	if got, err := FindAppDir(root, "apps/api"); err != nil || got != project {
		t.Fatalf("from the project: got %s (%v), want %s", got, err, project)
	}
	if got, err := FindAppDir(root, "apps/other"); err != nil || got != other {
		t.Fatalf("another name still walks: got %s (%v), want %s", got, err, other)
	}
	t.Chdir(t.TempDir())
	if got, err := FindAppDir(root, "apps/api"); err != nil || got != copyDir {
		t.Fatalf("a working directory outside the workspace is ignored: got %s (%v), want %s", got, err, copyDir)
	}
}

// TestWorkspaceProjectKeysNamesEveryProject: each project answers to its
// putnami.json name and its directory; session copies are not walked.
func TestWorkspaceProjectKeysNamesEveryProject(t *testing.T) {
	root := t.TempDir()
	write := func(dir, name string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, dir, "putnami.json"), []byte(`{"name":"`+name+`"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("apps/auth-server", "apps/auth-server")
	write("apps/cache", "cache-server")
	write(".claude/worktrees/copy/old", "auth/server")
	keys := WorkspaceProjectKeys(root)
	for _, key := range []string{"apps/auth-server", "cache-server", "apps/cache"} {
		if !keys[key] {
			t.Errorf("keys miss %q: %v", key, keys)
		}
	}
	if keys["auth/server"] || len(keys) != 3 {
		t.Fatalf("keys = %v, want the two projects only", keys)
	}
}
