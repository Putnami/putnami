package agentsession

import (
	"context"
	"errors"
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
