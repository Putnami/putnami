package gittest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/sdk/extension/gitread"
)

// plainFixture commits one file the way a fixture that reads the caller's git
// environment does, and returns its directory. It reports a git failure
// instead of ending the test.
func plainFixture(t *testing.T) (string, error) {
	t.Helper()
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "fixture.txt"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"commit", "-q", "-m", "fixture"}} {
		cmd := exec.Command(gitread.Executable(), args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			return repo, fmt.Errorf("git %v: %w: %s", args, err, out)
		}
	}
	return repo, nil
}

// isolatedFixture commits one file and tags it through Run, and returns its
// directory.
func isolatedFixture(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "fixture.txt"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	Run(t, repo, "init", "-q")
	Run(t, repo, "add", "-A")
	Run(t, repo, "commit", "-q", "-m", "fixture")
	Run(t, repo, "tag", "-a", "-m", "release", "v1")
	return repo
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath(gitread.Executable()); err != nil {
		t.Skip("git is not installed")
	}
}

// TestForeignMachineBreaksAPlainFixture proves the foreign machine is foreign:
// a fixture that keeps the caller's git environment fails under it.
func TestForeignMachineBreaksAPlainFixture(t *testing.T) {
	requireGit(t)
	if _, err := plainFixture(t); err != nil {
		t.Fatalf("the plain fixture fails on this machine: %v", err)
	}
	ForeignMachine(t)
	if _, err := plainFixture(t); err == nil {
		t.Fatal("a plain fixture committed under the foreign machine; the machine is not foreign")
	}
}

// TestDescribeTellsAPlainFixtureFromAnIsolatedOne: the foreign machine's
// global ignore file leaves a plain init and add with an empty index, and
// Describe shows it.
func TestDescribeTellsAPlainFixtureFromAnIsolatedOne(t *testing.T) {
	requireGit(t)
	build := func() string {
		repo := t.TempDir()
		if err := os.WriteFile(filepath.Join(repo, "fixture.txt"), []byte("fixture\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{{"init", "-q", "--initial-branch=main"}, {"add", "-A"}} {
			cmd := exec.Command(gitread.Executable(), args...)
			cmd.Dir = repo
			cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_DIR="+filepath.Join(repo, ".git"))
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, out)
			}
		}
		return repo
	}
	want := Describe(t, build())
	if !strings.Contains(want, "fixture.txt") {
		t.Fatalf("the index of this machine's fixture misses fixture.txt:\n%s", want)
	}
	ForeignMachine(t)
	if got := Describe(t, build()); got == want {
		t.Fatalf("Describe gives the same repository under the foreign machine:\n%s", got)
	}
}

// TestAssertMachineIndependentPassesAnIsolatedFixture commits, tags and reads
// a fixture through Run, here and under the foreign machine.
func TestAssertMachineIndependentPassesAnIsolatedFixture(t *testing.T) {
	requireGit(t)
	AssertMachineIndependent(t, func() string { return isolatedFixture(t) })

	repo := isolatedFixture(t)
	if got := Run(t, repo, "symbolic-ref", "--short", "HEAD"); got != DefaultBranch {
		t.Fatalf("branch = %q, want %q", got, DefaultBranch)
	}
	if got := Run(t, repo, "log", "-1", "--format=%an <%ae> %cn <%ce>"); got != Name+" <"+Email+"> "+Name+" <"+Email+">" {
		t.Fatalf("identity = %q", got)
	}
	if got := Run(t, repo, "rev-parse", "--absolute-git-dir"); filepath.Clean(filepath.FromSlash(got)) != filepath.Join(evalSymlinks(t, repo), ".git") {
		t.Fatalf("git dir = %q, want the fixture's", got)
	}
	if got := Run(t, repo, "check-attr", "-a", "fixture.txt"); got != "" {
		t.Fatalf("attributes = %q, want none", got)
	}
}

// TestDescribeNamesADirectoryWithoutARepository.
func TestDescribeNamesADirectoryWithoutARepository(t *testing.T) {
	requireGit(t)
	if got := Describe(t, t.TempDir()); !strings.HasPrefix(got, "no repository at the fixture") {
		t.Fatalf("Describe = %q", got)
	}
}

// TestEnvironDropsInheritedGitVariables keeps no GIT_ variable of the caller.
func TestEnvironDropsInheritedGitVariables(t *testing.T) {
	for _, entry := range Environ([]string{"GIT_INDEX_FILE=/elsewhere/index", "PATH=/bin"}) {
		if strings.HasPrefix(entry, "GIT_INDEX_FILE=") {
			t.Fatalf("Environ kept %q", entry)
		}
	}
	if env := Environ([]string{"PATH=/bin"}); env[0] != "PATH=/bin" {
		t.Fatalf("Environ dropped PATH: %v", env)
	}
}

// TestEnvLetsAnAppendedIdentityWin: exec keeps the last of two values.
func TestEnvLetsAnAppendedIdentityWin(t *testing.T) {
	requireGit(t)
	repo := t.TempDir()
	Run(t, repo, "init", "-q")
	cmd := Command(repo, "commit", "-q", "--allow-empty", "-m", "fixture")
	cmd.Env = append(cmd.Env, "GIT_AUTHOR_NAME=Fixture")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
	if got := Run(t, repo, "log", "-1", "--format=%an"); got != "Fixture" {
		t.Fatalf("author = %q, want Fixture", got)
	}
}

func evalSymlinks(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}
