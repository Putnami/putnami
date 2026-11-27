//go:build linux

package credentialcustody

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.putnami.dev/tooling/cli/internal/cli/clitest"
)

// A run that overstays its deadline fails with every process of its tree, and
// none of them outlives the wait: not a child that ignores SIGQUIT, nor a root
// that ignores it and so survives the dumps, which waitEngine then kills
// itself. The root prints the pids of its children once both run, so the test
// knows the processes the error must list and the kill must end.
func TestAHungEngineRunFailsWithItsTreeAndLeavesNoProcess(t *testing.T) {
	t.Parallel()
	clitest.RequireShell(t)
	cmd := exec.Command("/bin/sh", "-c", `trap "" QUIT
(trap "" QUIT; exec sleep 371) &
echo $!
sleep 372 &
echo $!
wait`)
	cmd.WaitDelay = engineDumpGrace
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pids := []int{cmd.Process.Pid}
	lines := bufio.NewScanner(stdout)
	for range 2 {
		if !lines.Scan() {
			t.Fatalf("the root printed no child pid: %v", lines.Err())
		}
		pid, err := strconv.Atoi(strings.TrimSpace(lines.Text()))
		if err != nil {
			t.Fatalf("the root printed %q, want a child pid", lines.Text())
		}
		pids = append(pids, pid)
	}
	// The root holds the pipe until it ends; drain it so the wait is not held.
	go func() { _, _ = io.Copy(io.Discard, stdout) }()

	err = waitEngine(cmd, 500*time.Millisecond)
	var hang *engineHangError
	if !errors.As(err, &hang) || !strings.Contains(err.Error(), "within 500ms") {
		t.Fatalf("waitEngine = %v; want the hang with its tree", err)
	}
	for _, pid := range pids {
		if !strings.Contains(err.Error(), fmt.Sprintf("pid=%d ", pid)) {
			t.Errorf("the hang error does not list pid %d:\n%v", pid, err)
		}
	}
	for _, pid := range pids {
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			if syscall.Kill(pid, 0) != nil {
				break
			}
			if state, ok := readTreeProcess(pid); ok && state.state == "Z" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("pid %d outlived the hung run", pid)
			}
		}
	}
}

// A run that ends within its deadline returns its own outcome.
func TestAnEngineRunWithinItsDeadlineReturnsItsExit(t *testing.T) {
	t.Parallel()
	clitest.RequireShell(t)
	cmd := exec.Command("/bin/sh", "-c", "exit 3")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var exitErr *exec.ExitError
	if err := waitEngine(cmd, time.Minute); !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 {
		t.Fatalf("waitEngine = %v; want exit 3", err)
	}
}
