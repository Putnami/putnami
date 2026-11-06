package gitread_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/sdk/extension/gitread"
	"go.putnami.dev/sdk/extension/gittest"
)

// initGitRepo creates a repository on branch main with one commit.
func initGitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath(gitread.Executable()); err != nil {
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
	out, err := gittest.Command(dir, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// TestInitGitRepoIsTheSameOnAForeignMachine: the fixture of this package's
// tests reads none of the caller's git environment.
func TestInitGitRepoIsTheSameOnAForeignMachine(t *testing.T) {
	gittest.AssertMachineIndependent(t, func() string {
		dir := initGitRepo(t)
		writeCommit(t, dir, "src/a.go", "feat: a", "The body.")
		return dir
	})
}

// A failed call names the verb, the directory and what git wrote on standard
// error.
func TestRunNamesTheVerbTheDirectoryAndGitsMessage(t *testing.T) {
	repo := initGitRepo(t)
	_, err := gitread.Run(repo, "rev-parse", "--verify", "--quiet", "missing^{commit}")
	if err == nil {
		t.Fatal("Run(rev-parse missing) succeeded, want an error")
	}
	if !strings.HasPrefix(err.Error(), "git rev-parse (in "+repo+"): ") {
		t.Fatalf("error = %q, want it to name the verb and the directory", err)
	}
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
