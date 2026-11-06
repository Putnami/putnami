package agentsession

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/gittest"
)

// featureBranchRepo is an empty repository on branch feat/x.
func featureBranchRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gittest.Run(t, dir, "init", "-q", "-b", "feat/x")
	return dir
}

func TestFeatureBranchRepoIsTheSameOnAForeignMachine(t *testing.T) {
	gittest.AssertMachineIndependent(t, func() string { return featureBranchRepo(t) })
}

func TestTheLaunchEnvironmentReadsTheProcess(t *testing.T) {
	spectest.Proves(t, sessionFeature, targetRequirement, "the-repository-comes-from-origin-without-host-or-credential")
	home := t.TempDir()
	t.Setenv("PUTNAMI_HOME", home)
	env := NewEnv("/work/repo", nil)
	if env.Home != home || env.sessionsDir() != filepath.Join(home, "contributor", "sessions") || env.Host != nil {
		t.Errorf("env %+v", env)
	}
	t.Setenv("PUTNAMI_HOME", "")
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	if env := NewEnv("/work/repo", nil); env.Home == "" {
		t.Error("no Putnami home")
	}

	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("git is not installed: %v", err)
	}
	dir := featureBranchRepo(t)
	if branch, err := runGit(context.Background(), dir, "symbolic-ref", "--short", "-q", "HEAD"); err != nil || branch != "feat/x" {
		t.Errorf("branch %q, %v", branch, err)
	}
	if _, err := runGit(context.Background(), dir, "remote", "get-url", "origin"); err == nil {
		t.Error("a missing origin answered")
	}
}

func TestSleepEndsWithItsContext(t *testing.T) {
	spectest.Proves(t, sessionFeature, budgetRequirement, "the-budgets-fit-the-declared-timeouts")
	if err := sleepContext(context.Background(), 0); err != nil {
		t.Error(err)
	}
	if err := sleepContext(context.Background(), time.Millisecond); err != nil {
		t.Error(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepContext(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("a canceled sleep answered %v", err)
	}
}

// detachedRepo is a repository whose HEAD is detached at its only commit.
func detachedRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "feat/x"},
		{"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "x"},
		{"checkout", "-q", "--detach"},
	} {
		gittest.Run(t, dir, args...)
	}
	return dir
}

func TestDetachedRepoIsTheSameOnAForeignMachine(t *testing.T) {
	gittest.AssertMachineIndependent(t, func() string { return detachedRepo(t) })
}

// A real git tells a detached HEAD, its status 1, from a git stopped at its
// caller's deadline: the first is no branch, the second an error.
func TestAGitStoppedByItsCallerIsNotADetachedHEAD(t *testing.T) {
	spectest.Proves(t, sessionFeature, targetRequirement, "a-git-failure-is-not-a-detached-head")
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("git is not installed: %v", err)
	}
	dir := detachedRepo(t)
	env := NewEnv(dir, nil)
	if branch, err := env.checkoutBranch(context.Background(), dir); branch != "" || err != nil {
		t.Errorf("detached HEAD: branch %q, %v", branch, err)
	}

	// The same checkout behind a git that says it started, then never ends:
	// its caller ends while it runs.
	bin := t.TempDir()
	started := filepath.Join(bin, "started")
	if out, err := exec.Command("mkfifo", started).CombinedOutput(); err != nil {
		t.Fatalf("mkfifo: %v: %s", err, out)
	}
	script := "#!/bin/sh\necho started > '" + started + "'\nexec /bin/sleep 3600\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		// Opening the FIFO waits for the git to open it for writing.
		if fifo, err := os.Open(started); err == nil {
			_, _ = io.ReadAll(fifo)
			_ = fifo.Close()
		}
		cancel()
	}()
	branch, err := env.checkoutBranch(ctx, dir)
	var exit gitExit
	if branch != "" || !errors.Is(err, context.Canceled) || errors.As(err, &exit) {
		t.Errorf("stopped git: branch %q, %v, want the caller's error", branch, err)
	}
}
