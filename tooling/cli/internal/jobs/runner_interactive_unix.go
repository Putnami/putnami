//go:build !windows

package jobs

import (
	"os/exec"
	"syscall"
	"time"
)

// runInteractive runs cmd, an interactive job that shares the CLI's terminal,
// and returns what cmd.Run returns. startJob starts it: it calls its argument,
// which starts cmd, or fails.
func runInteractive(cmd *exec.Cmd, startJob func(start func() error) error) error {
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
	cmd.WaitDelay = 5 * time.Second

	if err := startJob(cmd.Start); err != nil {
		return err
	}
	return cmd.Wait()
}
