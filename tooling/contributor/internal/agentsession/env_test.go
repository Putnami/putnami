package agentsession

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
)

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
	dir := t.TempDir()
	if _, err := runGit(context.Background(), dir, "init", "-q", "-b", "feat/x"); err != nil {
		t.Fatal(err)
	}
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

// A real git tells a detached HEAD, its status 1, from a git that outlives
// gitTimeout: the first is no branch, the second an error.
func TestAGitPastItsTimeoutIsNotADetachedHEAD(t *testing.T) {
	spectest.Proves(t, sessionFeature, targetRequirement, "a-git-failure-is-not-a-detached-head")
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("git is not installed: %v", err)
	}
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "feat/x"},
		{"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "x"},
		{"checkout", "-q", "--detach"},
	} {
		if _, err := runGit(context.Background(), dir, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	env := NewEnv(dir, nil)
	if branch, err := env.checkoutBranch(context.Background(), dir); branch != "" || err != nil {
		t.Errorf("detached HEAD: branch %q, %v", branch, err)
	}

	// The same checkout behind a git that sleeps past gitTimeout.
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\n/bin/sleep 3\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	branch, err := env.checkoutBranch(context.Background(), dir)
	var exit gitExit
	if branch != "" || err == nil || errors.As(err, &exit) {
		t.Errorf("slow git: branch %q, %v, want a timeout error", branch, err)
	}
}
