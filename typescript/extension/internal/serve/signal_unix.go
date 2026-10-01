//go:build unix

package serve

import (
	"errors"
	"syscall"
)

// sendSignal delivers sig to the process pid alone. A process that is already
// gone is not an error: its wait error is on its way, and a SIGKILL sent after
// a failed SIGTERM could reach another process that reused the pid.
func sendSignal(pid int, sig syscall.Signal) error {
	err := syscall.Kill(pid, sig)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}
