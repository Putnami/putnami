package credentialcustody

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// engineDeadline bounds one engine run of a scenario. A run passes in seconds
// alone and in under twenty seconds beside four busy loops and other race
// test packages on four CPUs; the bound stays well under the package's test
// timeout, so a run that hangs fails its own test with the process tree it was
// waiting on instead of ending the whole package.
const engineDeadline = 3 * time.Minute

// engineDumpGrace is how long a hung engine tree gets to write its goroutine
// dumps before it is killed.
const engineDumpGrace = 10 * time.Second

// treeProcess is one process of a hung engine's tree.
type treeProcess struct {
	pid, ppid          int
	state, wchan, role string
	args               string
}

// engineDumpPause is how long the engine's descendants get to write their
// dumps through it before the engine is asked for its own.
const engineDumpPause = time.Second

// waitEngine waits for cmd, started, within deadline. A run that
// overstays it is described, asked for its goroutine dumps, and killed with
// every process of its tree; the error then names the deadline and every
// process the run was waiting on, and output holds the dumps.
func waitEngine(cmd *exec.Cmd, deadline time.Duration) error {
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(deadline):
	}
	tree := engineTree(cmd.Process.Pid)
	select {
	case err := <-done:
		// The run ended as the deadline did: it did not hang.
		return err
	default:
	}
	lines := make([]string, 0, len(tree))
	for _, process := range tree {
		lines = append(lines, process.String())
	}
	dumpTree(tree)
	ended := false
	select {
	case <-done:
		ended = true
	case <-time.After(engineDumpGrace):
	}
	// A process the engine started outlives it once the engine is reaped, so
	// the kill takes the processes read before the dumps, not a new tree.
	for _, process := range tree {
		if found, err := os.FindProcess(process.pid); err == nil {
			_ = found.Kill()
		}
	}
	if !ended {
		_ = cmd.Process.Kill()
		<-done
	}
	return &engineHangError{deadline: deadline, tree: lines}
}

// engineHangError is an engine run that overstayed its deadline, with the
// process tree it left.
type engineHangError struct {
	deadline time.Duration
	tree     []string
}

func (e *engineHangError) Error() string {
	return fmt.Sprintf("the engine did not finish within %s; its process tree was:\n  %s", e.deadline, strings.Join(e.tree, "\n  "))
}
