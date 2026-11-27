//go:build !windows

package sessionreporter

import (
	"os/exec"
	"syscall"
)

// detachSession starts cmd in a session of its own: no terminal hangup, and
// no signal sent to the run's process group, reaches it.
func detachSession(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
