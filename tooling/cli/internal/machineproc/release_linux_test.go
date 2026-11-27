package machineproc

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// processState is the state letter /proc/<pid>/stat gives the process pid,
// and false once no process has that pid: it ended and was reaped.
func processState(t *testing.T, pid int) (byte, bool) {
	t.Helper()
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false
	}
	if err != nil {
		t.Fatal(err)
	}
	// The state is the first field after the command's ")".
	rest := string(data)
	if i := strings.LastIndexByte(rest, ')'); i >= 0 {
		rest = rest[i+1:]
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 || len(fields[0]) != 1 {
		t.Fatalf("unreadable stat of %d: %q", pid, data)
	}
	return fields[0][0], true
}

// eventually reports whether done holds of pid's state within ten seconds.
func eventually(t *testing.T, pid int, done func(state byte, exists bool) bool) bool {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if done(processState(t, pid)) {
			return true
		}
	}
	return false
}

// startExiting starts a child that exits at once.
func startExiting(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Skipf("no shell to start: %v", err)
	}
	return cmd
}

// TestAWorkerReapsTheDetachedChildItLetsGo pins ReleaseDetached: a CLI
// process releases a detached child, which stays a zombie of its parent until
// something waits it, and a worker, which outlives its runs, waits it so
// none stays a zombie (CLI ADR 0116 §4).
func TestAWorkerReapsTheDetachedChildItLetsGo(t *testing.T) {
	released := startExiting(t)
	pid := released.Process.Pid
	if err := ReleaseDetached(released); err != nil {
		t.Fatal(err)
	}
	if !eventually(t, pid, func(state byte, exists bool) bool { return exists && state == 'Z' }) {
		t.Fatalf("a child released outside a worker did not stay a zombie of this process")
	}
	// The test reaps that one itself, by its pid.
	var status syscall.WaitStatus
	if _, err := syscall.Wait4(pid, &status, 0, nil); err != nil {
		t.Fatal(err)
	}

	inWorker = true
	defer func() { inWorker = false }()
	waited := startExiting(t)
	pid = waited.Process.Pid
	if err := ReleaseDetached(waited); err != nil {
		t.Fatal(err)
	}
	if !eventually(t, pid, func(_ byte, exists bool) bool { return !exists }) {
		state, _ := processState(t, pid)
		t.Fatalf("a worker's detached child %d was not reaped: state %c", pid, state)
	}
}
