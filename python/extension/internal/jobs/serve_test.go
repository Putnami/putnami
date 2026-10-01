package jobs

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

func TestServe_SkipOnEmptyProject(t *testing.T) {
	ctx := &pctx.Context{
		Project: pctx.Project{Name: ""},
	}
	events := captureEvents(t, func(emit *jsonl.Emitter) {
		status, _, err := Serve(ctx, emit, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if status != "SKIP" {
			t.Errorf("expected SKIP, got %s", status)
		}
	})
	if len(events) != 0 {
		t.Errorf("expected no events for skip, got %d", len(events))
	}
}

func TestServe_FlagDefaults(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"no flags", nil},
		{"custom entrypoint", []string{"--entrypoint", "app.py"}},
		{"custom port", []string{"--port", "8080"}},
		{"no-watch", []string{"--no-watch"}},
		{"short watch flag", []string{"-w"}},
		{"combined", []string{"--entrypoint", "app.py", "--port", "9000", "--no-watch"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := &pctx.Context{
				Project: pctx.Project{Name: ""},
				Params:  pctx.Params{},
			}
			status, _, err := Serve(ctx, jsonl.New(), tt.args)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if status != "SKIP" {
				t.Errorf("expected SKIP, got %s", status)
			}
		})
	}
}

func TestServe_SyncWorkspaceFailure(t *testing.T) {
	ctx := &pctx.Context{
		WorkspaceRoot: "/nonexistent/path",
		Project: pctx.Project{
			Name:     "mypkg",
			Path:     "pkg",
			FullPath: "/nonexistent/path/pkg",
		},
		Params: pctx.Params{},
	}

	events := captureEvents(t, func(emit *jsonl.Emitter) {
		status, _, err := Serve(ctx, emit, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if status != "FAILED" {
			t.Errorf("expected FAILED, got %s", status)
		}
	})

	if len(events) == 0 {
		t.Error("expected events from failed sync")
	}
}

// ---- spawnServer / stopProcess / runServer / wait ----

func lookPathOrSkip(t *testing.T, name string) string {
	t.Helper()
	p, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s not available", name)
	}
	return p
}

func TestSpawnServer_Success(t *testing.T) {
	emit := jsonl.New()
	trueBin := lookPathOrSkip(t, "true")
	// Use a command that exits immediately
	proc, err := spawnServer(emit, []string{trueBin}, t.TempDir(), os.Environ())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	proc.wait()
	// Process should have exited
	if proc.cmd.ProcessState == nil {
		t.Error("expected process to have exited")
	}
}

func TestSpawnServer_InvalidCommand(t *testing.T) {
	emit := jsonl.New()
	_, err := spawnServer(emit, []string{"/nonexistent/binary"}, t.TempDir(), os.Environ())
	if err == nil {
		t.Error("expected error for invalid command")
	}
}

func TestSpawnServer_WithOutput(t *testing.T) {
	emit := jsonl.New()
	echoBin := lookPathOrSkip(t, "echo")
	proc, err := spawnServer(emit, []string{echoBin, "hello"}, t.TempDir(), os.Environ())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	proc.wait()
	if !proc.cmd.ProcessState.Success() {
		t.Error("expected process to succeed")
	}
}

func TestStopProcess_AlreadyExited(t *testing.T) {
	emit := jsonl.New()
	trueBin := lookPathOrSkip(t, "true")
	proc, err := spawnServer(emit, []string{trueBin}, t.TempDir(), os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	proc.wait()
	// Should not panic when process already exited
	stopProcess(proc, terminateServer)
}

func TestStopProcess_RunningProcess(t *testing.T) {
	emit := jsonl.New()
	// Use sleep which will be running when we stop it
	sleepBin := lookPathOrSkip(t, "sleep")
	proc, err := spawnServer(emit, []string{sleepBin, "60"}, t.TempDir(), os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	// Give it a moment to start
	time.Sleep(50 * time.Millisecond)
	stopProcess(proc, terminateServer)
	// Process should now be done
	if proc.cmd.ProcessState == nil {
		t.Error("expected process to have exited after stop")
	}
}

func TestRunServer_SuccessfulExit(t *testing.T) {
	emit := jsonl.New()
	sigChan := make(chan os.Signal, 1)
	trueBin := lookPathOrSkip(t, "true")
	exitCode := runServer(emit, []string{trueBin}, t.TempDir(), os.Environ(), sigChan)
	if exitCode != 0 {
		t.Errorf("expected exit code 0, got %d", exitCode)
	}
}

func TestRunServer_FailedExit(t *testing.T) {
	emit := jsonl.New()
	sigChan := make(chan os.Signal, 1)
	falseBin := lookPathOrSkip(t, "false")
	exitCode := runServer(emit, []string{falseBin}, t.TempDir(), os.Environ(), sigChan)
	if exitCode == 0 {
		t.Error("expected non-zero exit code")
	}
}

func TestRunServer_InvalidCommand(t *testing.T) {
	emit := jsonl.New()
	sigChan := make(chan os.Signal, 1)
	exitCode := runServer(emit, []string{"/nonexistent/binary"}, t.TempDir(), os.Environ(), sigChan)
	if exitCode != 1 {
		t.Errorf("expected exit code 1, got %d", exitCode)
	}
}

func TestRunServer_WithSpecificExitCode(t *testing.T) {
	// bash -c "exit 42" gives exit code 42
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	emit := jsonl.New()
	sigChan := make(chan os.Signal, 1)
	exitCode := runServer(emit, []string{"bash", "-c", "exit 42"}, t.TempDir(), os.Environ(), sigChan)
	if exitCode != 42 {
		t.Errorf("expected exit code 42, got %d", exitCode)
	}
}

// TestServe_WatchExitDetectionRace exercises the watch loop's exit-detection
// path: a ticker-style goroutine polls process exit state concurrently with the
// goroutine that owns cmd.Wait(). Previously the loop read cmd.ProcessState
// directly, racing with cmd.Wait()'s write of that field; this test runs that
// interaction so `go test -race` fails if the race regresses.
func TestServe_WatchExitDetectionRace(t *testing.T) {
	emit := jsonl.New()
	// A command that runs briefly then exits, so the poller observes the
	// not-exited -> exited transition while cmd.Wait() is still in flight.
	sleepBin := lookPathOrSkip(t, "sleep")
	proc, err := spawnServer(emit, []string{sleepBin, "0.2"}, t.TempDir(), os.Environ())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	pollDone := make(chan struct{})
	go func() {
		defer close(pollDone)
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for range ticker.C {
			if proc.exited() {
				return
			}
		}
	}()

	select {
	case <-pollDone:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for process exit to be detected")
	}

	proc.wait()
	if proc.waitErr != nil {
		t.Errorf("expected clean exit, got %v", proc.waitErr)
	}
}

func TestServe_NoWatchMode(t *testing.T) {
	tmp := setupPythonWorkspace(t, "testpkg")
	pkgDir := filepath.Join(tmp, "pkg")

	// Create a Python entrypoint
	os.WriteFile(filepath.Join(pkgDir, "src", "main.py"), []byte("print('hello')\n"), 0644)

	ctx := &pctx.Context{
		WorkspaceRoot: tmp,
		Project: pctx.Project{
			Name:     "testpkg",
			Path:     "pkg",
			FullPath: pkgDir,
		},
		Params: pctx.Params{},
	}

	events := captureEvents(t, func(emit *jsonl.Emitter) {
		status, _, err := Serve(ctx, emit, []string{"--no-watch"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// Either OK (server ran and exited) or FAILED (uv issue or script error)
		if status != "OK" && status != "FAILED" {
			t.Errorf("expected OK or FAILED, got %s", status)
		}
	})

	if len(events) == 0 {
		t.Error("expected JSONL events")
	}
}
