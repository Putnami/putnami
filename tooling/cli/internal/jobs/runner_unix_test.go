//go:build unix

package jobs

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"go.putnami.dev/sdk/extension/proctree"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

func TestRunJob_DoesNotHangWhenBackgroundChildKeepsStdoutOpen(t *testing.T) {
	shortenProcessGroupDelays(t)
	for _, mode := range []string{"exit", "cancel"} {
		t.Run(mode, func(t *testing.T) { testJobDescendantCleanup(t, mode, false) })
	}
}

// A descendant that ignores SIGTERM and no longer holds the job's stdout or
// stderr ends with the job, however the job ends: once the root exited, runJob
// kills what is left of the job's process group before it returns.
func TestRunJob_EndsDescendantThatIgnoresTermAndLeftTheJobPipes(t *testing.T) {
	shortenProcessGroupDelays(t)
	for _, mode := range []string{"exit", "cancel", "timeout"} {
		t.Run(mode, func(t *testing.T) { testJobDescendantCleanup(t, mode, true) })
	}
}

// stopProcessGroup returns at once when the group is empty or the tree never
// started, so a job that leaves nothing behind pays no delay.
func TestStopProcessGroup_ReturnsAtOnceWhenNothingRuns(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable on this platform")
	}
	exitedCmd := exec.Command("/bin/sh", "-c", "exit 0")
	exited := proctree.New(exitedCmd)
	if err := exited.Start(); err != nil {
		t.Fatal(err)
	}
	if err := exitedCmd.Wait(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = exited.Close() }()
	trees := map[string]*proctree.Tree{
		"exited":    exited,
		"unstarted": proctree.New(exec.Command("/bin/sh", "-c", "exit 0")),
	}
	for name, tree := range trees {
		t.Run(name, func(t *testing.T) {
			done := make(chan struct{})
			go func() {
				stopProcessGroup(tree, time.Hour)
				close(done)
			}()
			// A failure watchdog only: a correct stop returns without waiting.
			select {
			case <-done:
			case <-time.After(30 * time.Second):
				t.Fatal("stopProcessGroup waited for a group with no process")
			}
		})
	}
}

// shortenProcessGroupDelays makes runJob give up on a stop request after
// 100ms instead of seconds, for the test's duration.
func shortenProcessGroupDelays(t *testing.T) {
	t.Helper()
	oldDrainDelay := orphanPipeDrainDelay
	oldKillDelay := processGroupKillDelay
	orphanPipeDrainDelay = 100 * time.Millisecond
	processGroupKillDelay = 100 * time.Millisecond
	t.Cleanup(func() {
		orphanPipeDrainDelay = oldDrainDelay
		processGroupKillDelay = oldKillDelay
	})
}

// testJobDescendantCleanup runs a job whose root starts a background
// descendant that ignores SIGTERM, then ends the job by mode: "exit" (the root
// exits 0), "cancel" (the caller cancels), or "timeout" (the job's deadline
// expires). The descendant keeps the job's stdout and stderr, or, with
// leavePipes, writes to /dev/null instead. The test passes once the
// descendant is dead.
func testJobDescendantCleanup(t *testing.T, mode string, leavePipes bool) {
	t.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable on this platform")
	}
	wsRoot := t.TempDir()
	readyPath := filepath.Join(wsRoot, "ready.fifo")
	livenessPath := filepath.Join(wsRoot, "liveness.fifo")
	for _, path := range []string{readyPath, livenessPath} {
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Keep the FIFO open until the job returns. The child's extra writer then
	// proves its death by EOF, even if the OS has not reaped its zombie yet.
	keeper, err := os.OpenFile(livenessPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer keeper.Close() //nolint:errcheck // also closed before the EOF assertion
	liveness, err := os.Open(livenessPath)
	if err != nil {
		t.Fatal(err)
	}
	defer liveness.Close() //nolint:errcheck // test-owned pipe cleanup

	scriptPath := filepath.Join(wsRoot, "leaky-background.sh")
	// The descendant installs its TERM handler and, when asked, leaves the
	// job's pipes before it reports ready; the root writes its first event only
	// after that report.
	script := `#!/bin/sh
(
  trap '' TERM
  if [ "$DESCENDANT_OUTPUT" = detached ]; then exec >/dev/null 2>&1; fi
  exec 3>"$LIVENESS_FIFO"
  printf alive >&3
  printf 'ready\n' >"$READY_FIFO"
  while :; do sleep 1; done
) &
read ready <"$READY_FIFO"
if [ "$JOB_MODE" != exit ]; then
  printf '%s\n' '{"v":2,"type":"log","data":{"level":"info","message":"child ready"}}'
  while :; do sleep 1; done
fi
printf '%s\n' '{"v":2,"type":"result","data":{"status":"success"}}'
exit 0
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write leaky script: %v", err)
	}
	descendantOutput := "job-pipes"
	if leavePipes {
		descendantOutput = "detached"
	}

	ws := &workspace.Workspace{Root: wsRoot, Name: "test-ws"}
	project := &workspace.Project{ID: "/pkg", Name: "pkg", Path: "."}
	job := &ScheduledJob{
		Project:   project,
		Extension: &extension.ExtensionDescription{Name: "@putnami/test", Path: wsRoot},
		JobDef: &extension.JobDefinition{
			ExtensionName: "@putnami/test",
			Name:          "lint~leaky",
			Kind:          "command",
			Command:       scriptPath,
			TimeoutMs:     unboundedJobTimeoutMs,
			Env: map[string]string{
				"READY_FIFO": readyPath, "LIVENESS_FIFO": livenessPath, "JOB_MODE": mode,
				"DESCENDANT_OUTPUT": descendantOutput,
			},
		},
	}

	// stop ends the run the way mode does. A timeout is the job's context
	// expiring with context.DeadlineExceeded, which runJob reads exactly as it
	// reads its own deadline; no wall clock decides when it fires.
	var ctx context.Context
	var stop func()
	if mode == "timeout" {
		expiring := newExpiringContext()
		ctx, stop = expiring, expiring.expire
	} else {
		cancelable, cancel := context.WithCancel(t.Context())
		ctx, stop = cancelable, cancel
	}
	defer stop()
	var pgid atomic.Int64
	ctx = WithProcessGroupObserver(ctx, func(id int) {
		pgid.Store(int64(id))
		if ctx.Err() != nil {
			_ = proctree.KillGroup(id)
		}
	})
	defer func() { _ = proctree.KillGroup(int(pgid.Load())) }()
	// This is only a failure watchdog. Success depends on event ordering and
	// descendant EOF, not on startup or scheduling fitting a small wall budget.
	watchdogDone := make(chan struct{})
	watchdog := time.AfterFunc(30*time.Second, func() {
		stop()
		_ = proctree.KillGroup(int(pgid.Load()))
		_ = liveness.Close()
		close(watchdogDone)
	})
	defer func() {
		if !watchdog.Stop() {
			<-watchdogDone
			t.Error("the job's descendant outlived the job until the failure watchdog killed it")
		}
	}()

	result, err := RunJob(ctx, ws, job, nil, nil, nil, func(event RawJobEvent) {
		if mode != "exit" && event.Type == "log" {
			stop()
		}
	})
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	if !result.FirstEventObserved {
		t.Fatal("job never acknowledged the child's installed TERM handler and chosen output")
	}
	switch mode {
	case "exit":
		if ctx.Err() != nil {
			t.Fatalf("job required cancellation to return: %v", ctx.Err())
		}
		if result.Status != "success" {
			t.Fatalf("status = %q, want success", result.Status)
		}
	case "cancel":
		if result.Status != "canceled" {
			t.Fatalf("status = %q, want canceled", result.Status)
		}
	case "timeout":
		if result.Status != "failed" || !result.TimedOut || result.Error == nil {
			t.Fatalf("result = status %q, timed out %v, error %v; want a failed, timed-out job with its error",
				result.Status, result.TimedOut, result.Error)
		}
	}
	_ = keeper.Close()
	if got, err := io.ReadAll(liveness); err != nil || string(got) != "alive" {
		t.Fatalf("descendant liveness = %q, %v; want alive followed by EOF", got, err)
	}
}
