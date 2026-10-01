package git

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// outsideRepository returns a directory git discovery cannot escape, so the
// answer does not depend on where the test's temporary directory lives.
func outsideRepository(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(dir))
	return dir
}

// fakeGit puts a git program that prints stderr and exits with code first on
// PATH, standing in for a git that fails in a way the test names.
func fakeGit(t *testing.T, stderr string, code int) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in git is a POSIX shell script")
	}
	bin := t.TempDir()
	script := "#!/bin/sh\ncat >&2 <<'EOF'\n" + stderr + "\nEOF\nexit " + strconv.Itoa(code) + "\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+"/bin"+string(os.PathListSeparator)+"/usr/bin")
}

func TestUnmanaged_InsideRepositoryIsNil(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)
	if err := Unmanaged(dir); err != nil {
		t.Fatalf("Unmanaged(repository) = %v, want nil", err)
	}
	// A repository with no commit is still one Git manages.
	unborn := t.TempDir()
	runGit(t, unborn, "init")
	if err := Unmanaged(unborn); err != nil {
		t.Fatalf("Unmanaged(repository with no commit) = %v, want nil", err)
	}
}

func TestUnmanaged_NotRepository(t *testing.T) {
	spectest.Proves(t, "cli/workspace-without-git", "git-history-is-refused-in-one-line",
		"a-missing-repository-and-a-missing-program-are-told-apart")
	dir := outsideRepository(t)

	err := Unmanaged(dir)
	if !errors.Is(err, ErrNotRepository) || errors.Is(err, ErrNoProgram) {
		t.Fatalf("Unmanaged(plain directory) = %v, want ErrNotRepository", err)
	}
	want := dir + " is not a git repository; run git init there and commit"
	if err.Error() != want {
		t.Fatalf("message = %q, want %q", err.Error(), want)
	}
}

func TestUnmanaged_NoProgram(t *testing.T) {
	spectest.Proves(t, "cli/workspace-without-git", "git-history-is-refused-in-one-line",
		"a-missing-repository-and-a-missing-program-are-told-apart")
	dir := t.TempDir()
	t.Setenv("PATH", t.TempDir())

	err := Unmanaged(dir)
	if !errors.Is(err, ErrNoProgram) || errors.Is(err, ErrNotRepository) {
		t.Fatalf("Unmanaged(no git on PATH) = %v, want ErrNoProgram", err)
	}
	want := "no git program is on PATH; install git, then run git init in " + dir + " and commit"
	if err.Error() != want {
		t.Fatalf("message = %q, want %q", err.Error(), want)
	}
}

func TestUnmanaged_DeveloperToolsStubIsNoProgram(t *testing.T) {
	spectest.Proves(t, "cli/workspace-without-git", "git-history-is-refused-in-one-line",
		"a-missing-repository-and-a-missing-program-are-told-apart")
	for _, notice := range []string{
		"xcrun: error: invalid active developer path (/Library/Developer/CommandLineTools), missing xcrun at: /Library/Developer/CommandLineTools/usr/bin/xcrun",
		"xcode-select: note: No developer tools were found, requesting install.",
	} {
		t.Run(strings.SplitN(notice, ":", 2)[0], func(t *testing.T) {
			fakeGit(t, notice, 1)
			if err := Unmanaged(t.TempDir()); !errors.Is(err, ErrNoProgram) {
				t.Fatalf("Unmanaged(developer tools stub) = %v, want ErrNoProgram", err)
			}
		})
	}
}

// A git failure that is neither a missing program nor a missing repository
// keeps its own detail: Unmanaged does not claim it.
func TestUnmanaged_OtherGitFailureIsNotClassified(t *testing.T) {
	spectest.Proves(t, "cli/workspace-without-git", "git-history-is-refused-in-one-line",
		"another-git-failure-keeps-its-detail")
	for name, stderr := range map[string]string{
		"dubious ownership": "fatal: detected dubious ownership in repository at '/work'",
		"corrupt index":     "fatal: index file corrupt",
	} {
		t.Run(name, func(t *testing.T) {
			fakeGit(t, stderr, 128)
			if err := Unmanaged(t.TempDir()); err != nil {
				t.Fatalf("Unmanaged(%s) = %v, want nil", name, err)
			}
		})
	}
}

func TestUnmanaged_MessageIsOneLine(t *testing.T) {
	t.Parallel()
	for _, cause := range []error{ErrNoProgram, ErrNotRepository} {
		message := (&UnmanagedError{Dir: "/work/app", Cause: cause}).Error()
		if strings.ContainsAny(message, "\r\n") {
			t.Errorf("message for %v spans several lines: %q", cause, message)
		}
		if !strings.Contains(message, "git init") {
			t.Errorf("message for %v does not name the command to run: %q", cause, message)
		}
		if strings.Contains(message, "exit status") {
			t.Errorf("message for %v leaks the raw git failure: %q", cause, message)
		}
	}
}

func TestIsShallow_OutsideRepositoryNamesGit(t *testing.T) {
	spectest.Proves(t, "cli/workspace-without-git", "git-history-is-refused-in-one-line",
		"version-outside-a-repository-names-git")
	dir := outsideRepository(t)

	_, err := IsShallow(dir)
	if !errors.Is(err, ErrNotRepository) {
		t.Fatalf("IsShallow(plain directory) = %v, want ErrNotRepository", err)
	}
	want := "version and publish need git history: " + dir + " is not a git repository; run git init there and commit"
	if err.Error() != want {
		t.Fatalf("message = %q, want %q", err.Error(), want)
	}
}

func TestIsShallow_NoProgramNamesGit(t *testing.T) {
	spectest.Proves(t, "cli/workspace-without-git", "git-history-is-refused-in-one-line",
		"a-missing-repository-and-a-missing-program-are-told-apart")
	dir := t.TempDir()
	t.Setenv("PATH", t.TempDir())

	_, err := IsShallow(dir)
	if !errors.Is(err, ErrNoProgram) {
		t.Fatalf("IsShallow(no git on PATH) = %v, want ErrNoProgram", err)
	}
	if strings.Contains(err.Error(), "exit status") || strings.Contains(err.Error(), "executable file not found") {
		t.Fatalf("message leaks the raw failure: %q", err.Error())
	}
}

// A failure Git reports for a repository it does manage keeps its detail.
func TestIsShallow_OtherGitFailureKeepsItsDetail(t *testing.T) {
	spectest.Proves(t, "cli/workspace-without-git", "git-history-is-refused-in-one-line",
		"another-git-failure-keeps-its-detail")
	fakeGit(t, "fatal: detected dubious ownership in repository at '/work'", 128)

	_, err := IsShallow(t.TempDir())
	if err == nil || errors.Is(err, ErrNotRepository) || errors.Is(err, ErrNoProgram) {
		t.Fatalf("IsShallow(dubious ownership) = %v, want the git failure", err)
	}
	if !strings.Contains(err.Error(), "dubious ownership") {
		t.Fatalf("message dropped the git failure: %q", err.Error())
	}
}
