package git

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func lookPathAt(path string) func(string) (string, error) {
	return func(string) (string, error) { return path, nil }
}

func always(string) bool { return true }

func TestExecutableLooksAgainWhenTheForwardedGitIsGone(t *testing.T) {
	cache := &sync.Map{}
	paths := []string{"/Applications/Xcode-old.app/git", "/Applications/Xcode.app/git"}
	calls := 0
	find := func() string { calls++; return paths[calls-1] }
	gone := func(path string) bool { return path != paths[0] }
	if got := resolveExecutable(cache, "darwin", lookPathAt(appleGitTrampoline), "", find, always); got != paths[0] {
		t.Fatalf("got %q, want %q", got, paths[0])
	}
	if got := resolveExecutable(cache, "darwin", lookPathAt(appleGitTrampoline), "", find, gone); got != "git" {
		t.Fatalf("a removed git: got %q, want the trampoline", got)
	}
	if got := resolveExecutable(cache, "darwin", lookPathAt(appleGitTrampoline), "", find, gone); got != paths[1] {
		t.Fatalf("after a removed git: got %q, want %q", got, paths[1])
	}
}

func TestExecutableAsksXcrunOnceForConcurrentFirstCalls(t *testing.T) {
	cache := &sync.Map{}
	var calls atomic.Int32
	release := make(chan struct{})
	find := func() string { calls.Add(1); <-release; return "/Applications/Xcode.app/git" }
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { resolveExecutable(cache, "darwin", lookPathAt(appleGitTrampoline), "", find, always) })
	}
	close(release)
	wg.Wait()
	if got := calls.Load(); got != 1 {
		t.Errorf("xcrun asked %d times, want once", got)
	}
}

func TestExecutableIsGitOffMacOS(t *testing.T) {
	find := func() string { t.Fatal("xcrun asked off macOS"); return "" }
	for _, goos := range []string{"linux", "windows"} {
		if got := resolveExecutable(&sync.Map{}, goos, lookPathAt(appleGitTrampoline), "", find, always); got != "git" {
			t.Errorf("%s: got %q, want git", goos, got)
		}
	}
}

func TestExecutableKeepsAGitEarlierOnPath(t *testing.T) {
	find := func() string { t.Fatal("xcrun asked for a git that is not the trampoline"); return "" }
	if got := resolveExecutable(&sync.Map{}, "darwin", lookPathAt("/opt/homebrew/bin/git"), "", find, always); got != "git" {
		t.Errorf("got %q, want git", got)
	}
	missing := func(string) (string, error) { return "", exec.ErrNotFound }
	if got := resolveExecutable(&sync.Map{}, "darwin", missing, "", find, always); got != "git" {
		t.Errorf("no git on PATH: got %q, want git", got)
	}
}

func TestExecutableRunsTheGitTheTrampolineForwardsTo(t *testing.T) {
	cache := &sync.Map{}
	calls := 0
	find := func() string { calls++; return "/Library/Developer/CommandLineTools/usr/bin/git" }
	for range 3 {
		if got := resolveExecutable(cache, "darwin", lookPathAt(appleGitTrampoline), "", find, always); got != "/Library/Developer/CommandLineTools/usr/bin/git" {
			t.Fatalf("got %q, want the developer tools' git", got)
		}
	}
	if calls != 1 {
		t.Errorf("xcrun asked %d times, want once per developer directory", calls)
	}
	resolveExecutable(cache, "darwin", lookPathAt(appleGitTrampoline), "/Applications/Xcode.app/Contents/Developer", find, always)
	if calls != 2 {
		t.Errorf("another developer directory asked xcrun %d times in total, want 2", calls)
	}
}

func TestExecutableFallsBackToGitWhenXcrunNamesNone(t *testing.T) {
	cache := &sync.Map{}
	calls := 0
	find := func() string { calls++; return "" }
	for range 2 {
		if got := resolveExecutable(cache, "darwin", lookPathAt(appleGitTrampoline), "", find, always); got != "git" {
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

// startedByName lists the files that start git by name on purpose, with how
// many times.
var startedByName = map[string]int{
	// git worktree add runs repository hooks and filters, which keep the
	// environment the macOS trampoline gives them.
	"internal/commands/cachecmd/cache_verify.go": 1,
}

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
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if n := len(directGitCall.FindAll(data, -1)); n != startedByName[rel] {
			found = append(found, fmt.Sprintf("%s: %d by name, %d allowed", rel, n, startedByName[rel]))
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
