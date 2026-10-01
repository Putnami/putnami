//go:build windows

package jobs

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// interactiveTreeHelperEnv names the directory where one level of the helper
// job records itself. Level 1 is the job, which starts level 2 and records
// whether it shares a console with the test.
const (
	interactiveTreeHelperEnv      = "PUTNAMI_JOBS_INTERACTIVE_TREE_HELPER_DIR"
	interactiveTreeHelperLevelEnv = "PUTNAMI_JOBS_INTERACTIVE_TREE_HELPER_LEVEL"
)

// TestInteractiveTreeHelperProcess is not a test: it is one level of the job
// the interactive tests below run, and it returns at once unless they started
// it.
func TestInteractiveTreeHelperProcess(t *testing.T) {
	dir := os.Getenv(interactiveTreeHelperEnv)
	if dir == "" {
		return
	}
	level := os.Getenv(interactiveTreeHelperLevelEnv)
	if level == "" {
		level = "1"
		if _, err := windows.GetConsoleCP(); err == nil {
			if os.WriteFile(filepath.Join(dir, "console"), nil, 0o644) != nil {
				os.Exit(2)
			}
		}
		child := exec.Command(os.Args[0], "-test.run=^TestInteractiveTreeHelperProcess$")
		child.Env = append(os.Environ(), interactiveTreeHelperLevelEnv+"=2")
		if child.Start() != nil {
			os.Exit(2)
		}
	}
	if os.WriteFile(filepath.Join(dir, "level-"+level), []byte(strconv.Itoa(os.Getpid())), 0o644) != nil {
		os.Exit(2)
	}
	time.Sleep(10 * time.Minute)
	os.Exit(3)
}

// A canceled interactive job ends with every process it started: the job runs
// as the root of a process tree whose Job Object holds its descendants. It
// keeps the console the CLI has. The levels are checked the moment the run
// returns, with no wait: Windows ends a job's processes asynchronously, and the
// run returns only after closing the tree, which waits until they exited
// (proctree's Close).
func TestRunJobInteractive_CancelEndsTheWholeTree(t *testing.T) {
	dir := t.TempDir()
	ws := &workspace.Workspace{Root: dir, Name: "test-ws"}
	job := &ScheduledJob{
		Project:   &workspace.Project{ID: "test-ws", Name: "test-ws", Path: "."},
		Extension: &extension.ExtensionDescription{Name: "@putnami/test", Path: dir},
		JobDef: &extension.JobDefinition{
			ExtensionName: "@putnami/test",
			Name:          "demo-tree",
			Kind:          "command",
			Command:       os.Args[0],
			// The job context arguments follow "--", which the test binary
			// leaves alone.
			Args:      []string{"-test.run=^TestInteractiveTreeHelperProcess$", "--"},
			Env:       map[string]string{interactiveTreeHelperEnv: dir},
			TimeoutMs: unboundedJobTimeoutMs,
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type outcome struct {
		result *JobResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		var stdout, stderr strings.Builder
		result, err := RunJobInteractiveWithStreams(ctx, ws, job, nil, nil, nil, strings.NewReader(""), &stdout, &stderr)
		done <- outcome{result, err}
	}()

	pids := make([]int, 0, 2)
	for _, level := range []string{"level-1", "level-2"} {
		pid := waitForPidFile(t, filepath.Join(dir, level))
		pids = append(pids, pid)
		t.Cleanup(func() { terminateTestProcess(pid) })
	}
	_, consoleErr := windows.GetConsoleCP()
	if _, err := os.Stat(filepath.Join(dir, "console")); (consoleErr == nil) != (err == nil) {
		t.Errorf("the job shares a console = %v, want %v as the test", err == nil, consoleErr == nil)
	}

	cancel()
	var got outcome
	select {
	case got = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("RunJobInteractiveWithStreams did not return after the cancel")
	}
	if got.err != nil {
		t.Fatalf("RunJobInteractiveWithStreams: %v", got.err)
	}
	if got.result.Status != "canceled" {
		t.Errorf("status = %q, want canceled", got.result.Status)
	}
	for i, pid := range pids {
		if testProcessRunning(pid) {
			t.Errorf("level %d (pid %d) outlived the canceled interactive job", i+1, pid)
		}
	}
}

// waitForPidFile returns the pid a helper level recorded at path.
func waitForPidFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if data, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(string(data)); err == nil {
				return pid
			}
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("%s was never recorded", filepath.Base(path))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// testProcessRunning reports whether pid names a process that has not exited.
func testProcessRunning(pid int) bool {
	process, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer func() { _ = windows.CloseHandle(process) }()
	event, err := windows.WaitForSingleObject(process, 0)
	return err != nil || event != windows.WAIT_OBJECT_0
}

// terminateTestProcess ends pid if it still runs.
func terminateTestProcess(pid int) {
	process, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return
	}
	_ = windows.TerminateProcess(process, 1)
	_ = windows.CloseHandle(process)
}
