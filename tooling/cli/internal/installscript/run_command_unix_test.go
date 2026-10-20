//go:build unix

package installscript

import (
	"os/exec"
	"syscall"
	"testing"
)

// withoutControllingTerminal starts cmd in a new session, so /dev/tty cannot
// be opened and the installer must give the command /dev/null as its stdin.
func withoutControllingTerminal(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// inOwnProcessGroup starts cmd in a process group of its own, so it outlives
// the group of the process that starts it.
func inOwnProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
