//go:build unix

package clitest

import (
	"os/exec"
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
