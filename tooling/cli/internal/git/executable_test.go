package git

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

func lookPathAt(path string) func(string) (string, error) {
	return func(string) (string, error) { return path, nil }
}

func TestExecutableIsGitOffMacOS(t *testing.T) {
	find := func() string { t.Fatal("xcrun asked off macOS"); return "" }
	for _, goos := range []string{"linux", "windows"} {
		if got := resolveExecutable(goos, lookPathAt(appleGitTrampoline), "linux-"+goos, find); got != "git" {
			t.Errorf("%s: got %q, want git", goos, got)
		}
	}
}

func TestExecutableKeepsAGitEarlierOnPath(t *testing.T) {
	find := func() string { t.Fatal("xcrun asked for a git that is not the trampoline"); return "" }
	if got := resolveExecutable("darwin", lookPathAt("/opt/homebrew/bin/git"), t.Name(), find); got != "git" {
		t.Errorf("got %q, want git", got)
	}
	missing := func(string) (string, error) { return "", exec.ErrNotFound }
	if got := resolveExecutable("darwin", missing, t.Name(), find); got != "git" {
		t.Errorf("no git on PATH: got %q, want git", got)
	}
}

func TestExecutableRunsTheGitTheTrampolineForwardsTo(t *testing.T) {
	calls := 0
	find := func() string { calls++; return "/Library/Developer/CommandLineTools/usr/bin/git" }
	for range 3 {
		if got := resolveExecutable("darwin", lookPathAt(appleGitTrampoline), t.Name(), find); got != "/Library/Developer/CommandLineTools/usr/bin/git" {
			t.Fatalf("got %q, want the developer tools' git", got)
		}
	}
	if calls != 1 {
		t.Errorf("xcrun asked %d times, want once per developer directory", calls)
	}
	resolveExecutable("darwin", lookPathAt(appleGitTrampoline), t.Name()+"-other", find)
	if calls != 2 {
		t.Errorf("another developer directory asked xcrun %d times in total, want 2", calls)
	}
}

func TestExecutableFallsBackToGitWhenXcrunNamesNone(t *testing.T) {
	calls := 0
	find := func() string { calls++; return "" }
	for range 2 {
		if got := resolveExecutable("darwin", lookPathAt(appleGitTrampoline), t.Name(), find); got != "git" {
			t.Fatalf("got %q, want git", got)
		}
	}
	if calls != 1 {
		t.Errorf("xcrun asked %d times after a failure, want once", calls)
	}
}

func TestExecutableRunsAWorkingGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	path := Executable()
	if runtime.GOOS == "darwin" && path != "git" && !filepath.IsAbs(path) {
		t.Fatalf("Executable() = %q, want git or an absolute path", path)
	}
	out, err := exec.Command(path, "--version").Output()
	if err != nil {
		t.Fatalf("%s --version: %v", path, err)
	}
	if len(out) == 0 {
		t.Fatalf("%s --version wrote nothing", path)
	}
}

// A git started by name runs the macOS trampoline again. Tests are left
// out: a split package may not import this one.
var directGitCall = regexp.MustCompile(`exec\.Command(Context)?\(([A-Za-z_][A-Za-z0-9_]*, )?"git"`)

func TestTheModuleRunsGitThroughExecutable(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".")) && path != root {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if directGitCall.Match(data) {
			rel, _ := filepath.Rel(root, path)
			found = append(found, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) > 0 {
		t.Errorf("start git as exec.Command(git.Executable(), ...), not by name:\n  %s", strings.Join(found, "\n  "))
	}
}
