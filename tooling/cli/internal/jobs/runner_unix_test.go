//go:build unix

package jobs

import (
	"context"
	"io"
	"os"
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
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable on this platform")
	}

	oldDrainDelay := orphanPipeDrainDelay
	oldKillDelay := processGroupKillDelay
	orphanPipeDrainDelay = 100 * time.Millisecond
	processGroupKillDelay = 100 * time.Millisecond
	defer func() {
		orphanPipeDrainDelay = oldDrainDelay
		processGroupKillDelay = oldKillDelay
	}()
	for _, mode := range []string{"exit", "cancel"} {
		t.Run(mode, func(t *testing.T) { testJobOrphanPipeCleanup(t, mode) })
	}
}

func testJobOrphanPipeCleanup(t *testing.T, mode string) {
	t.Helper()
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
	script := `#!/bin/sh
(
  trap '' TERM
  exec 3>"$LIVENESS_FIFO"
  printf alive >&3
  printf 'ready\n' >"$READY_FIFO"
  while :; do sleep 1; done
) &
read ready <"$READY_FIFO"
if [ "$JOB_MODE" = cancel ]; then
  printf '%s\n' '{"v":2,"type":"log","data":{"level":"info","message":"child ready"}}'
  while :; do sleep 1; done
fi
printf '%s\n' '{"v":2,"type":"result","data":{"status":"success"}}'
exit 0
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write leaky script: %v", err)
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
			},
		},
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
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
		cancel()
		_ = proctree.KillGroup(int(pgid.Load()))
		_ = liveness.Close()
		close(watchdogDone)
	})
	defer func() {
		if !watchdog.Stop() {
			<-watchdogDone
			t.Error("orphan-pipe cleanup needed the failure watchdog")
		}
	}()

	result, err := RunJob(ctx, ws, job, nil, nil, nil, func(event RawJobEvent) {
		if mode == "cancel" && event.Type == "log" {
			cancel()
		}
	})
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	if !result.FirstEventObserved {
		t.Fatal("job never acknowledged the child's installed TERM handler and open pipes")
	}
	wantStatus := "success"
	if mode == "cancel" {
		wantStatus = "canceled"
	} else if ctx.Err() != nil {
		t.Fatalf("job required cancellation to return: %v", ctx.Err())
	}
	if result.Status != wantStatus {
		t.Fatalf("status = %q, want %s", result.Status, wantStatus)
	}
	_ = keeper.Close()
	if got, err := io.ReadAll(liveness); err != nil || string(got) != "alive" {
		t.Fatalf("descendant liveness = %q, %v; want alive followed by EOF", got, err)
	}
}
