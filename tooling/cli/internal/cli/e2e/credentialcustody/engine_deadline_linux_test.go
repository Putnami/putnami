//go:build linux

package credentialcustody

import (
	"errors"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.putnami.dev/tooling/cli/internal/cli/clitest"
)

// A run that overstays its deadline fails with every process of its tree,
// and none of them outlives the wait, not even one that ignores SIGQUIT.
func TestAHungEngineRunFailsWithItsTreeAndLeavesNoProcess(t *testing.T) {
	t.Parallel()
	clitest.RequireShell(t)
	cmd := exec.Command("/bin/sh", "-c", `(trap "" QUIT; exec sleep 371) & sleep 372 & wait`)
	cmd.WaitDelay = engineDumpGrace
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	tree := engineTree(cmd.Process.Pid)
	err := waitEngine(cmd, 500*time.Millisecond)
	var hang *engineHangError
	if !errors.As(err, &hang) || !strings.Contains(err.Error(), "within 500ms") || !strings.Contains(err.Error(), "sleep 371") {
		t.Fatalf("waitEngine = %v; want the hang with its tree", err)
	}
	for _, process := range tree {
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			if syscall.Kill(process.pid, 0) != nil {
				break
			}
			if state, ok := readTreeProcess(process.pid); ok && state.state == "Z" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s outlived the hung run", process)
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
