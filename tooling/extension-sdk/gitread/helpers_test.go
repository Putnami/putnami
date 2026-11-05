package gitread

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// initGitRepo creates a repository on branch main with one commit.
func initGitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
		{"config", "commit.gpgsign", "false"},
		{"checkout", "-b", "main"},
	} {
		runGit(t, dir, args...)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Test"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "initial")
	return dir
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(Executable(), args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// writeCommit writes one file and commits it with a subject and, when body is
// not empty, a body.
func writeCommit(t *testing.T, dir, relPath, subject, body string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(subject), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", ".")
	if body == "" {
		runGit(t, dir, "commit", "-m", subject)
		return
	}
	runGit(t, dir, "commit", "-m", subject, "-m", body)
}
