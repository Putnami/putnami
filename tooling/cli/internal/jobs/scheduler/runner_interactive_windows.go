//go:build windows

package scheduler

import (
	"os"
	"os/exec"
	"sync/atomic"
	"syscall"
	"time"

	"go.putnami.dev/sdk/extension/proctree"
)

// runInteractive runs cmd, an interactive job that shares the CLI's console,
// as the root of a process tree (decision D-W9), and returns what cmd.Run
// would return. The tree's Job Object holds every process the job starts, so
// none outlives the job.
//
// The tree keeps the CLI's console: a Windows console has no foreground
// process group, so the job reads the console input and writes to the console
// as the CLI does. The tree runs in a process group of its own, so a Ctrl+C
// typed at the console reaches the CLI, which cancels the job: the cancel asks
// the tree to exit with CTRL_BREAK_EVENT and ends the whole tree when it still
// runs after interactiveStopBudget, so the job stops on its own terms within
// the budget. A second signal to the CLI ends the tree at once. Closing the
// tree once the root exited ends every process the job left behind and returns
// once they exited, so no process of the job outlives the run. startJob starts
// it: it calls its argument, which starts the tree, or fails.
//
// forced reports that the tree was killed: a second signal, or a cancel the
// tree outlived by the stop budget. A Windows exit status names no signal, so
// the runner remembers its own kills.
func runInteractive(cmd *exec.Cmd, startJob func(start func() error) error) (forced bool, err error) {
	tree := proctree.New(cmd)
	waitDone := make(chan struct{})
	var canceledAt atomic.Int64
	cmd.Cancel = func() error {
		canceledAt.Store(time.Now().UnixNano())
		err := tree.Terminate()
		go forceKillProcessGroupAfter(waitDone, tree, interactiveStopBudget)
		return err
	}
	cmd.WaitDelay = interactiveStopBudget

	// A second signal to the CLI ends the tree at once, from before its start
	// on (interactiveHold).
	hold := holdInteractive(func() { _ = tree.Kill() })
	defer hold.release()
	if err := startJob(func() error { return hold.start(tree.Start) }); err != nil {
		return false, err
	}
	defer func() { _ = tree.Close() }()
	err = cmd.Wait()
	close(waitDone)
	hold.release()
	forced = hold.forced()
	// The budget's kill ends a tree that still ran once the budget passed, so
	// a wait that outlasted the budget after the cancel ended in that kill.
	if at := canceledAt.Load(); at != 0 && time.Since(time.Unix(0, at)) >= interactiveStopBudget {
		forced = true
	}
	return forced, err
}

// statusControlCExit is the exit code of a Windows process that a console
// control event ended (STATUS_CONTROL_C_EXIT).
const statusControlCExit = 0xC000013A

// exitedOnItsOwn reports whether the child ended by its own exit after a
// terminal signal: the runner killed no process of its tree, and the console
// control event of the cancel did not end it.
func exitedOnItsOwn(state *os.ProcessState, forced bool) bool {
	if state == nil || forced {
		return false
	}
	status, ok := state.Sys().(syscall.WaitStatus)
	return ok && status.ExitCode != statusControlCExit
}

// endedByInterrupt reports whether the child ended from an interrupt. Windows
// reports no signal in an exit status, so it never does: a Ctrl+C reaches the
// CLI, which cancels the job.
func endedByInterrupt(error) bool { return false }
