package jobs

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/proctree"
)

const (
	stopHelperEnv = "PUTNAMI_PYTHON_SERVE_STOP_HELPER"
	// stopHelperChildFileEnv makes the helper start a second helper first, as
	// uv starts python, and write that child's pid to the file it names.
	stopHelperChildFileEnv = "PUTNAMI_PYTHON_SERVE_STOP_HELPER_CHILD_FILE"
	// stopHelperOutliveDirEnv makes the helper ignore SIGTERM, write "ready"
	// in the directory it names, run stopHelperLifetime more, then write
	// "done" there and exit 3.
	stopHelperOutliveDirEnv = "PUTNAMI_PYTHON_SERVE_STOP_HELPER_OUTLIVE_DIR"
	stopHelperLifetime      = 300 * time.Millisecond
)

// TestServeStopHelperProcess is not a test: it is a server that runs until it
// is killed, and it returns at once unless a test started it as one.
func TestServeStopHelperProcess(t *testing.T) {
	if os.Getenv(stopHelperEnv) != "1" {
		return
	}
	if pidFile := os.Getenv(stopHelperChildFileEnv); pidFile != "" {
		child := exec.Command(os.Args[0], "-test.run=^TestServeStopHelperProcess$")
		child.Env = append(os.Environ(), stopHelperChildFileEnv+"=")
		if err := child.Start(); err != nil {
			os.Exit(4)
		}
		partial := pidFile + ".partial"
		if os.WriteFile(partial, []byte(strconv.Itoa(child.Process.Pid)), 0o644) != nil || os.Rename(partial, pidFile) != nil {
			os.Exit(5)
		}
	}
	if dir := os.Getenv(stopHelperOutliveDirEnv); dir != "" {
		signal.Ignore(syscall.SIGTERM)
		if os.WriteFile(filepath.Join(dir, "ready"), nil, 0o644) != nil {
			os.Exit(6)
		}
		time.Sleep(stopHelperLifetime)
		_ = os.WriteFile(filepath.Join(dir, "done"), nil, 0o644)
		os.Exit(3)
	}
	time.Sleep(10 * time.Minute)
	os.Exit(3)
}

// startStopHelper spawns a server that runs until it is killed.
func startStopHelper(t *testing.T, env ...string) *serverProcess {
	t.Helper()
	return startStopHelperOn(t, runtime.GOOS, env...)
}

// startStopHelperOn spawns a server that runs until it is killed, started the
// way it is on goos.
func startStopHelperOn(t *testing.T, goos string, env ...string) *serverProcess {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("locate the test binary: %v", err)
	}
	env = append(append(os.Environ(), stopHelperEnv+"=1"), env...)
	proc, err := spawnServerOn(goos, jsonl.New(), []string{executable, "-test.run=^TestServeStopHelperProcess$"}, t.TempDir(), env)
	if err != nil {
		t.Fatalf("spawn the server: %v", err)
	}
	t.Cleanup(func() {
		_ = proc.tree.Kill()
		_ = proc.cmd.Process.Kill()
		proc.wait()
	})
	return proc
}

// waitForChild returns the pid the helper wrote to pidFile, and ends that
// process when the test finishes.
func waitForChild(t *testing.T, pidFile string) int {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		data, err := os.ReadFile(pidFile)
		if err == nil {
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil {
				t.Fatalf("read the child pid: %v", err)
			}
			t.Cleanup(func() {
				if proctree.ProcessAlive(pid) {
					if process, err := os.FindProcess(pid); err == nil {
						_ = process.Kill()
					}
				}
			})
			return pid
		}
		if !time.Now().Before(deadline) {
			t.Fatal("the server did not start its child")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitForGone fails the test when pid still runs after a bound.
func waitForGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for proctree.ProcessAlive(pid) {
		if !time.Now().Before(deadline) {
			t.Fatalf("the server's child %d still runs after the stop", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// stopWithin runs stopProcessWithin and fails the test when it has not
// returned within bound.
func stopWithin(t *testing.T, proc *serverProcess, grace, bound time.Duration, ask func(*os.Process) error) {
	t.Helper()
	stopped := make(chan struct{})
	go func() {
		stopProcessWithin(proc, grace, ask)
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(bound):
		t.Fatalf("the stop did not return within %v", bound)
	}
	if !proc.exited() {
		t.Fatal("the stop returned while the server still runs")
	}
}

// A request that could not be delivered, as a signal to a single process on
// Windows, is followed by a kill at once instead of a wait for the grace.
func TestStopProcess_KillsAtOnceWhenTheRequestFails(t *testing.T) {
	proc := startStopHelper(t)
	stopWithin(t, proc, time.Hour, 10*time.Second, func(*os.Process) error {
		return errors.New("signal not delivered")
	})
	if proc.waitErr == nil {
		t.Fatal("the server exited cleanly, want it killed")
	}
}

// A server that does not exit after a delivered request is killed once the
// grace runs out.
func TestStopProcess_KillsAServerThatOutlivesTheGrace(t *testing.T) {
	proc := startStopHelper(t)
	stopWithin(t, proc, 50*time.Millisecond, 10*time.Second, func(*os.Process) error { return nil })
	if proc.waitErr == nil {
		t.Fatal("the server exited cleanly, want it killed")
	}
}

// On every OS but Windows the server is a plain child, started and stopped as
// before trees existed.
func TestStartServer_PlainChildOutsideWindows(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		proc := startStopHelperOn(t, goos)
		if proc.tree != nil {
			t.Errorf("%s: the server roots a process tree, want a plain child", goos)
		}
		stopWithin(t, proc, time.Hour, 10*time.Second, func(*os.Process) error {
			return errors.New("signal not delivered")
		})
	}
}

// On Windows the server roots a process tree, so a stop ends uv and the python
// it started, whatever the relayed request did. This runs the Windows start
// path on the host OS, where the tree is a process group.
func TestStopProcess_TreeServerEndsItsChild(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	proc := startStopHelperOn(t, "windows", stopHelperChildFileEnv+"="+pidFile)
	if proc.tree == nil {
		t.Fatal("the server started for windows roots no process tree")
	}
	child := waitForChild(t, pidFile)
	stopWithin(t, proc, 50*time.Millisecond, 30*time.Second, func(*os.Process) error {
		return errors.New("signal not delivered")
	})
	waitForGone(t, child)
}
