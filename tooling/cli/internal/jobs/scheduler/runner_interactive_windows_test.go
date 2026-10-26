//go:build windows

package scheduler

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"go.putnami.dev/protocol/features/spectest"
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

// exitCodeHelperEnv names the exit code TestExitCodeHelperProcess exits with.
const exitCodeHelperEnv = "PUTNAMI_JOBS_EXIT_CODE_HELPER"

// TestExitCodeHelperProcess is not a test: it exits with the code
// exitCodeHelperEnv names, and returns at once unless a test started it.
func TestExitCodeHelperProcess(t *testing.T) {
	code := os.Getenv(exitCodeHelperEnv)
	if code == "" {
		return
	}
	n, err := strconv.ParseUint(code, 10, 32)
	if err != nil {
		os.Exit(2)
	}
	os.Exit(int(int32(uint32(n))))
}

// exitedWith runs a process that exits with code and returns its state.
func exitedWith(t *testing.T, code uint32) *os.ProcessState {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestExitCodeHelperProcess$")
	cmd.Env = append(os.Environ(), exitCodeHelperEnv+"="+strconv.FormatUint(uint64(code), 10))
	_ = cmd.Run()
	if cmd.ProcessState == nil {
		t.Fatalf("the helper exiting with %#x never ran", code)
	}
	if got := cmd.ProcessState.Sys().(syscall.WaitStatus).ExitCode; got != code {
		t.Fatalf("the helper exited with %#x, want %#x", got, code)
	}
	return cmd.ProcessState
}

// A child that exits by itself keeps its exit code, 0 included, and a child
// that the console control event ended (STATUS_CONTROL_C_EXIT) or the runner
// killed did not exit on its own.
func TestExitedOnItsOwn_TellsAConsoleControlEventFromAnExit(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "an-interactive-command-stops-on-its-own-terms",
		"a-child-that-exits-by-itself-after-the-signal-keeps-its-exit-code")
	spectest.Proves(t, "cli/job-planning-execution", "an-interactive-command-stops-on-its-own-terms",
		"a-child-that-dies-by-the-signal-or-is-killed-is-interrupted")
	cases := []struct {
		name   string
		code   uint32
		forced bool
		want   bool
	}{
		{name: "exit 0", code: 0, want: true},
		{name: "exit 3", code: 3, want: true},
		{name: "STATUS_CONTROL_C_EXIT", code: statusControlCExit, want: false},
		{name: "killed by the runner", code: 1, forced: true, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := exitedOnItsOwn(exitedWith(t, tc.code), tc.forced); got != tc.want {
				t.Errorf("exitedOnItsOwn(exit %#x, forced %v) = %v, want %v", tc.code, tc.forced, got, tc.want)
			}
		})
	}
	if exitedOnItsOwn(nil, false) {
		t.Error("exitedOnItsOwn(nil) = true, want false for a child that never ran")
	}
}
