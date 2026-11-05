package gittest

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// hostileConfig is a global configuration that breaks every fixture commit
// that reads it: it signs commits and tags with a gpg that does not exist,
// rejects every commit with a hook, and names another default branch.
const hostileConfig = `[commit]
	gpgsign = true
[tag]
	gpgsign = true
[gpg]
	program = /nonexistent/gpg
[core]
	hooksPath = %HOOKS%
[init]
	defaultBranch = trunk
`

// withHostileMachine points every place git reads a global configuration from
// at hostileConfig, and GIT_DIR at a directory that is not the fixture.
func withHostileMachine(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	if runtime.GOOS == "windows" {
		t.Skip("the rejecting hook is a shell script")
	}
	home := t.TempDir()
	hooks := filepath.Join(home, "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, hook := range []string{"pre-commit", "commit-msg"} {
		if err := os.WriteFile(filepath.Join(hooks, hook), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	config := []byte(strings.ReplaceAll(hostileConfig, "%HOOKS%", hooks))
	global := filepath.Join(home, ".gitconfig")
	if err := os.WriteFile(global, config, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".config", "git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".config", "git", "config"), config, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_DIR", filepath.Join(home, "not-the-fixture"))
}

// TestHostileMachineBreaksAPlainGitCommit proves the machine of the other
// tests is hostile: a commit that reads its configuration fails.
func TestHostileMachineBreaksAPlainGitCommit(t *testing.T) {
	withHostileMachine(t)
	repo := t.TempDir()
	plain := func(args ...string) error {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_DIR="+filepath.Join(repo, ".git"),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		return cmd.Run()
	}
	if err := plain("init", "-q"); err != nil {
		t.Fatalf("git init: %v", err)
	}
	if err := plain("commit", "-q", "--allow-empty", "-m", "fixture"); err == nil {
		t.Fatal("a plain git commit passed under the hostile configuration; the test machine is not hostile")
	}
}

// TestCommandIgnoresTheMachineConfiguration commits, tags and reads a fixture
// under the hostile configuration, through Command.
func TestCommandIgnoresTheMachineConfiguration(t *testing.T) {
	withHostileMachine(t)
	repo := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		out, err := Command(repo, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q")
	run("commit", "-q", "--allow-empty", "-m", "fixture")
	run("tag", "-a", "-m", "release", "v1")

	if got := run("symbolic-ref", "--short", "HEAD"); got != DefaultBranch {
		t.Fatalf("branch = %q, want %q", got, DefaultBranch)
	}
	if got := run("log", "-1", "--format=%an <%ae> %cn <%ce>"); got != Name+" <"+Email+"> "+Name+" <"+Email+">" {
		t.Fatalf("identity = %q", got)
	}
	if got := run("rev-parse", "--absolute-git-dir"); got != filepath.Join(evalSymlinks(t, repo), ".git") {
		t.Fatalf("git dir = %q, want the fixture's", got)
	}
}

// TestEnvDropsInheritedGitVariables keeps no GIT_ variable of the process.
func TestEnvDropsInheritedGitVariables(t *testing.T) {
	t.Setenv("GIT_INDEX_FILE", "/elsewhere/index")
	for _, entry := range Env() {
		if strings.HasPrefix(entry, "GIT_INDEX_FILE=") {
			t.Fatalf("Env kept %q", entry)
		}
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
