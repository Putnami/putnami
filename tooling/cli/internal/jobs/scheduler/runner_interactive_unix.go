//go:build !windows

package scheduler

import (
	"errors"
	"os"
	"os/exec"
	"sync/atomic"
	"syscall"

	"go.putnami.dev/tooling/cli/internal/abort"
)

// runInteractive runs cmd, an interactive job that shares the CLI's terminal,
// and returns what cmd.Run returns. forced reports that a second signal killed
// the child. startJob starts it: it calls its argument, which starts cmd, or
// fails.
func runInteractive(cmd *exec.Cmd, startJob func(start func() error) error) (forced bool, err error) {
	// Do NOT place the child in its own process group (no Setpgid). An
	// interactive extension inherits the real terminal via os.Stdin, and only
	// the controlling terminal's foreground process group may read from it. The
	// CLI is that foreground group, so the child must stay in it — otherwise
	// the line discipline still echoes typed characters but the child's read()
	// on stdin blocks forever and prompts never receive their answer.
	// Sharing the group also lets a terminal Ctrl+C (SIGINT) reach the child
	// directly, since the kernel signals the whole foreground group.
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Signal only the child PID, never the process group: without Setpgid
		// the child shares the CLI's group, so syscall.Kill(-pgid, …) would
		// also terminate the CLI process itself.
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	// A canceled child stops on its own terms: it has interactiveStopBudget to
	// exit before Wait kills it. A terminal Ctrl+C reaches it directly, and the
	// cancel's SIGTERM reaches it when a supervisor signaled the CLI alone.
	cmd.WaitDelay = interactiveStopBudget

	if err := startJob(cmd.Start); err != nil {
		return false, err
	}
	// A second signal to the CLI kills the child at once.
	var killed atomic.Bool
	release := abort.HoldInteractive(func() {
		killed.Store(true)
		_ = cmd.Process.Kill()
	})
	err = cmd.Wait()
	release()
	return killed.Load(), err
}

// exitedOnItsOwn reports whether the child ended by its own exit after a
// terminal signal: it exited rather than died by a signal, and no second signal
// killed it. Every stop this process sends is a signal, so a child that died by
// one died by the terminal signal or was killed at the stop budget.
func exitedOnItsOwn(state *os.ProcessState, forced bool) bool {
	return state != nil && state.Exited() && !forced
}

// endedByInterrupt reports whether the child ended from SIGINT. A terminal
// Ctrl+C reaches the child directly, because it shares the CLI's foreground
// process group, and the child can exit before the CLI's own cancel lands.
func endedByInterrupt(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	return ok && status.Signaled() && status.Signal() == syscall.SIGINT
}
