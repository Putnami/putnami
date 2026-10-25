//go:build unix

package scheduler

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/abort"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// interactiveStopJob is an interactive job that runs script, a /bin/sh script,
// in a fresh workspace, with the path of a file the script writes once it
// runs.
func interactiveStopJob(t *testing.T, script string) (*workspace.Workspace, *ScheduledJob, string) {
	t.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable on this platform")
	}
	wsRoot := t.TempDir()
	scriptPath := filepath.Join(wsRoot, "job.sh")
	writeExecutable(t, scriptPath, "#!/bin/sh\nREADY="+filepath.Join(wsRoot, "ready")+"\nSTOPPED="+filepath.Join(wsRoot, "stopped")+"\n"+script)
	ws := &workspace.Workspace{Root: wsRoot, Name: "test-ws"}
	job := &ScheduledJob{
		Project:   &workspace.Project{ID: "test-ws", Name: "test-ws", Path: "."},
		Extension: &extension.ExtensionDescription{Name: "@putnami/test", Path: wsRoot},
		JobDef: &extension.JobDefinition{
			ExtensionName: "@putnami/test",
			Name:          "demo-stop",
			Kind:          "command",
			Command:       scriptPath,
			TimeoutMs:     unboundedJobTimeoutMs,
		},
	}
	return ws, job, wsRoot
}

// withInteractiveStopBudget runs the test with budget as the stop budget.
func withInteractiveStopBudget(t *testing.T, budget time.Duration) {
	t.Helper()
	previous := interactiveStopBudget
	interactiveStopBudget = budget
	t.Cleanup(func() { interactiveStopBudget = previous })
}

// startInteractive runs the job until ctx is canceled and returns the channel
// its result arrives on, once the script wrote its ready file.
func startInteractive(t *testing.T, ctx context.Context, ws *workspace.Workspace, job *ScheduledJob, wsRoot string) <-chan *JobResult {
	t.Helper()
	done := make(chan *JobResult, 1)
	go func() {
		res, err := RunJobInteractive(ctx, ws, job, nil, nil, nil)
		if err != nil {
			t.Errorf("RunJobInteractive: %v", err)
		}
		done <- res
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(wsRoot, "ready")); err == nil {
			return done
		}
		if time.Now().After(deadline) {
			t.Fatal("the interactive job never wrote its ready file")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// awaitResult returns the job's result, or fails after limit.
func awaitResult(t *testing.T, done <-chan *JobResult, limit time.Duration) *JobResult {
	t.Helper()
	select {
	case res := <-done:
		if res == nil {
			t.Fatal("RunJobInteractive returned no result")
		}
		return res
	case <-time.After(limit):
		t.Fatalf("the interactive job still runs after %s", limit)
		return nil
	}
}

// The core gives a canceled interactive child the 90 s budget the owner set,
// rather than the five seconds a streamed job gets.
func TestInteractiveStopBudgetIsNinetySeconds(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "an-interactive-command-stops-on-its-own-terms",
		"the-cli-waits-for-an-interactive-child-after-a-terminal-signal")
	if abort.InteractiveStopBudget != 90*time.Second || interactiveStopBudget != abort.InteractiveStopBudget {
		t.Fatalf("stop budget = %s (abort %s), want 90s", interactiveStopBudget, abort.InteractiveStopBudget)
	}
}

// A canceled child that takes longer than the old five-second delay would
// allow, scaled down, still finishes its own teardown: the runner waits for
// its exit within the budget instead of killing it. The child exited by
// itself, so its exit code 0 is the run's: the run succeeds.
func TestRunJobInteractive_CanceledChildStopsOnItsOwnTerms(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "an-interactive-command-stops-on-its-own-terms",
		"the-cli-waits-for-an-interactive-child-after-a-terminal-signal")
	spectest.Proves(t, "cli/job-planning-execution", "an-interactive-command-stops-on-its-own-terms",
		"a-child-that-exits-by-itself-after-the-signal-keeps-its-exit-code")
	withInteractiveStopBudget(t, 30*time.Second)
	ws, job, wsRoot := interactiveStopJob(t,
		"trap 'sleep 1; echo stopped > \"$STOPPED\"; exit 0' TERM\necho ready > \"$READY\"\nwhile :; do sleep 0.05; done\n")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := startInteractive(t, ctx, ws, job, wsRoot)
	cancel()
	res := awaitResult(t, done, 20*time.Second)

	if res.Status != "success" || res.ExitCode != 0 || res.Error != nil {
		t.Errorf("result = %+v, want the child's own success", res)
	}
	if _, err := os.Stat(filepath.Join(wsRoot, "stopped")); err != nil {
		t.Errorf("the child was killed before its own teardown finished: %v", err)
	}
	if abort.InteractiveHeld() {
		t.Error("the runner still holds the child after it exited")
	}
}

// A canceled child that exits by itself with a code of its own keeps it: the
// run fails with that code, and putnami does not report it as interrupted.
func TestRunJobInteractive_CanceledChildKeepsItsOwnExitCode(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "an-interactive-command-stops-on-its-own-terms",
		"a-child-that-exits-by-itself-after-the-signal-keeps-its-exit-code")
	withInteractiveStopBudget(t, 30*time.Second)
	ws, job, wsRoot := interactiveStopJob(t,
		"trap 'exit 3' TERM\necho ready > \"$READY\"\nwhile :; do sleep 0.05; done\n")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := startInteractive(t, ctx, ws, job, wsRoot)
	cancel()
	res := awaitResult(t, done, 20*time.Second)

	if res.Status != "failed" || res.ExitCode != 3 || res.TimedOut {
		t.Errorf("result = %+v, want the child's own exit code 3", res)
	}
}

// A canceled child that dies by the terminal signal has no exit code of its
// own: the run is canceled, which putnami reports as interrupted.
func TestRunJobInteractive_ChildThatDiesByTheSignalIsCanceled(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "an-interactive-command-stops-on-its-own-terms",
		"a-child-that-dies-by-the-signal-or-is-killed-is-interrupted")
	withInteractiveStopBudget(t, 30*time.Second)
	ws, job, wsRoot := interactiveStopJob(t,
		"echo ready > \"$READY\"\nwhile :; do sleep 0.05; done\n")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := startInteractive(t, ctx, ws, job, wsRoot)
	cancel()
	if res := awaitResult(t, done, 20*time.Second); res.Status != "canceled" {
		t.Errorf("result = %+v, want canceled", res)
	}
}

// A child that ignores the cancel is killed once the budget runs out.
func TestRunJobInteractive_ChildPastTheBudgetIsKilled(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "an-interactive-command-stops-on-its-own-terms",
		"the-cli-waits-for-an-interactive-child-after-a-terminal-signal")
	spectest.Proves(t, "cli/job-planning-execution", "an-interactive-command-stops-on-its-own-terms",
		"a-child-that-dies-by-the-signal-or-is-killed-is-interrupted")
	withInteractiveStopBudget(t, 200*time.Millisecond)
	ws, job, wsRoot := interactiveStopJob(t,
		"trap '' TERM\necho ready > \"$READY\"\nwhile :; do sleep 0.05; done\n")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := startInteractive(t, ctx, ws, job, wsRoot)
	cancel()
	if res := awaitResult(t, done, 10*time.Second); res.Status != "canceled" {
		t.Errorf("status = %q, want canceled", res.Status)
	}
}

// A second signal to the CLI kills the child at once, within the budget.
func TestRunJobInteractive_KillInteractiveEndsTheChildAtOnce(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "an-interactive-command-stops-on-its-own-terms",
		"a-second-signal-kills-the-interactive-child-at-once")
	spectest.Proves(t, "cli/job-planning-execution", "an-interactive-command-stops-on-its-own-terms",
		"a-child-that-dies-by-the-signal-or-is-killed-is-interrupted")
	withInteractiveStopBudget(t, time.Minute)
	ws, job, wsRoot := interactiveStopJob(t,
		"trap '' TERM\necho ready > \"$READY\"\nwhile :; do sleep 0.05; done\n")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := startInteractive(t, ctx, ws, job, wsRoot)
	if !abort.InteractiveHeld() {
		t.Fatal("the runner does not hold the running child")
	}
	cancel()
	abort.KillInteractive()
	if res := awaitResult(t, done, 10*time.Second); res.Status != "canceled" {
		t.Errorf("status = %q, want canceled", res.Status)
	}
}

// The runner holds the child from before its start: a first signal that lands
// while the child starts finds it held, so the CLI gives it the interactive
// shutdown bound, and a second one that lands then kills it as soon as it
// started.
func TestRunInteractive_HoldsTheChildFromBeforeItsStart(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "an-interactive-command-stops-on-its-own-terms",
		"the-cli-holds-the-interactive-child-from-before-its-start")
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable on this platform")
	}
	withInteractiveStopBudget(t, time.Minute)
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", "trap '' TERM; while :; do sleep 0.05; done")

	type outcome struct {
		heldBeforeStart bool
		forced          bool
		err             error
	}
	done := make(chan outcome, 1)
	go func() {
		var o outcome
		o.forced, o.err = runInteractive(cmd, func(start func() error) error {
			o.heldBeforeStart = abort.InteractiveHeld()
			abort.KillInteractive()
			return start()
		})
		done <- o
	}()
	var o outcome
	select {
	case o = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the child still runs 10s after a kill that landed while it started")
	}
	if !o.heldBeforeStart {
		t.Error("the runner did not hold the child before its start")
	}
	var exitErr *exec.ExitError
	if !o.forced || !errors.As(o.err, &exitErr) {
		t.Errorf("forced = %v, err = %v; want a forced kill", o.forced, o.err)
	}
	if abort.InteractiveHeld() {
		t.Error("the runner still holds the child after it exited")
	}
}
