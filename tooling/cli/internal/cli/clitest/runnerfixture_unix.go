//go:build unix

package clitest

import (
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// RequireRunnerFixture skips the test where the fixture provider cannot
// supervise a submitted execution. Unix delivers the supervisor's SIGTERM.
func RequireRunnerFixture(*testing.T) {}

// startDetachedSupervisor starts the supervisor in a session of its own, so
// the death of the RPC process and of its client leaves it running.
func startDetachedSupervisor(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Start()
}

// stopSupervisor asks the supervisor pid to cancel its attempt: it receives
// SIGTERM and terminates the engine's process tree itself.
func stopSupervisor(pid int) error {
	return syscall.Kill(pid, syscall.SIGTERM)
}

// ProcessExited reports whether pid has exited: it no longer exists, or it is
// a zombie its parent has not reaped yet. A ps that cannot run reports no exit.
func ProcessExited(pid int) bool {
	err := syscall.Kill(pid, 0)
	if errors.Is(err, syscall.ESRCH) {
		return true
	}
	if err != nil {
		return false
	}
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		// ps exits non-zero when the pid is gone.
		return true
	}
	return err == nil && strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}
