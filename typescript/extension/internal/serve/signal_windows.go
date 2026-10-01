//go:build windows

package serve

import (
	"fmt"
	"os"
	"syscall"
)

// sendSignal delivers sig to the process pid alone, which runs in the
// extension's console process group. SIGKILL ends it. SIGTERM relays the stop
// request the extension received, as proctree.Relay does, and sends nothing:
// the console control event that delivered the request reached every process
// of the group, bun included. Any other signal cannot be sent to a single
// Windows process.
func sendSignal(pid int, sig syscall.Signal) error {
	switch sig {
	case syscall.SIGTERM:
		return nil
	case syscall.SIGKILL:
		process, err := os.FindProcess(pid)
		if err != nil {
			return err
		}
		defer func() { _ = process.Release() }()
		return process.Kill()
	default:
		return fmt.Errorf("send %v to process %d: a single process cannot receive it on Windows", sig, pid)
	}
}
