//go:build windows

package jobs

import (
	"os/exec"
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
// the tree to exit with CTRL_BREAK_EVENT and ends the whole tree after
// processGroupKillDelay, as for a streamed job. Closing the tree once the root
// exited ends every process the job left behind and returns once they exited,
// so no process of the job outlives the run. startJob starts it: it calls its
// argument, which starts the tree, or fails.
func runInteractive(cmd *exec.Cmd, startJob func(start func() error) error) error {
	tree := proctree.New(cmd)
	waitDone := make(chan struct{})
	cmd.Cancel = func() error {
		err := tree.Terminate()
		go forceKillProcessGroupAfter(waitDone, tree, processGroupKillDelay)
		return err
	}
	cmd.WaitDelay = 5 * time.Second

	if err := startJob(tree.Start); err != nil {
		return err
	}
	defer func() { _ = tree.Close() }()
	err := cmd.Wait()
	close(waitDone)
	return err
}
