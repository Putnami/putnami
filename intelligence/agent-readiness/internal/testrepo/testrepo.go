// Package testrepo creates small git repositories for the command tests.
package testrepo

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// New returns the path of a git repository with a few commits in two areas.
func New(t testing.TB) string {
	t.Helper()
	dir := t.TempDir()
	run(t, dir, "init", "-q", "-b", "main")
	files := []struct{ path, content string }{
		{"README.md", "# sample\n"},
		{"services/api/main.go", "package main\n\nfunc main() {}\n"},
		{"services/api/main_test.go", "package main\n\nimport \"testing\"\n\nfunc TestMain(t *testing.T) {}\n"},
		{"web/app/index.ts", "export const app = 1;\n"},
	}
	for _, file := range files {
		Write(t, dir, file.path, file.content)
		run(t, dir, "add", file.path)
		run(t, dir, "commit", "-q", "-m", "add "+file.path)
	}
	return dir
}

// Write writes content to path inside the repository.
func Write(t testing.TB, dir, path, content string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Status returns `git status --porcelain --ignored` for the repository.
func Status(t testing.TB, dir string) string {
	t.Helper()
	return run(t, dir, "status", "--porcelain", "--ignored")
}

func run(t testing.TB, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Sample", "GIT_AUTHOR_EMAIL=sample@example.com",
		"GIT_COMMITTER_NAME=Sample", "GIT_COMMITTER_EMAIL=sample@example.com",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}
